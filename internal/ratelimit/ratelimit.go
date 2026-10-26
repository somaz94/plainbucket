// Package ratelimit caps the requests and body bytes of one client: a token bucket for
// requests, which are refused past it, and a shared schedule for bytes, which are slowed.
package ratelimit

import (
	"context"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// Limit is a client's allowance; zero Requests or Bytes does not limit.
type Limit struct {
	Requests float64 // per second
	Burst    int     // requests allowed at once; at least 1, and Requests rounded up by default
	Bytes    int64   // body bytes per second, request and response together
}

// Zero reports whether l limits nothing.
func (l Limit) Zero() bool { return l.Requests <= 0 && l.Bytes <= 0 }

func (l Limit) burst() float64 {
	if l.Burst > 0 {
		return float64(l.Burst)
	}
	return max(1, math.Ceil(l.Requests))
}

// Limiter enforces one Limit for every request of one client.
type Limiter struct {
	limit Limit
	now   func() time.Time

	mu     sync.Mutex
	tokens float64
	last   time.Time // of the last refill
	next   time.Time // when the bytes sent so far are paid for
	seen   time.Time // last use, for pruning
}

// New returns a Limiter starting with a full bucket.
func New(l Limit) *Limiter { return newAt(l, time.Now) }

func newAt(l Limit, now func() time.Time) *Limiter {
	t := now()
	return &Limiter{limit: l, now: now, tokens: l.burst(), last: t, seen: t}
}

// Allow takes one request; when the bucket is empty it returns false and how long until
// a request would be allowed.
func (lm *Limiter) Allow() (bool, time.Duration) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	now := lm.now()
	lm.seen = now
	if lm.limit.Requests <= 0 {
		return true, 0
	}
	lm.tokens = min(lm.limit.burst(), lm.tokens+now.Sub(lm.last).Seconds()*lm.limit.Requests)
	lm.last = now
	if lm.tokens >= 1 {
		lm.tokens--
		return true, 0
	}
	return false, time.Duration((1 - lm.tokens) / lm.limit.Requests * float64(time.Second))
}

// Wait holds the caller until n more body bytes fit the byte rate; concurrent requests of
// the client share it. Time not spent sending earns no burst.
func (lm *Limiter) Wait(ctx context.Context, n int) error {
	if lm.limit.Bytes <= 0 || n <= 0 {
		return ctx.Err()
	}
	lm.mu.Lock()
	now := lm.now()
	lm.seen = now
	if lm.next.Before(now) {
		lm.next = now
	}
	cost := time.Duration(float64(n) / float64(lm.limit.Bytes) * float64(time.Second))
	lm.next = lm.next.Add(cost)
	d := lm.next.Sub(now)
	lm.mu.Unlock()
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		// A client gone mid-wait does not leave its debt to the next request.
		lm.mu.Lock()
		lm.next = lm.next.Add(-cost)
		lm.mu.Unlock()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// throttleChunk is how many bytes are paid for at a time, so a slow rate still streams.
const throttleChunk = 32 << 10

// Reader slows r to the Limiter's byte rate.
func (lm *Limiter) Reader(ctx context.Context, r io.ReadCloser) io.ReadCloser {
	if lm.limit.Bytes <= 0 {
		return r
	}
	return &reader{ReadCloser: r, lm: lm, ctx: ctx}
}

type reader struct {
	io.ReadCloser
	lm  *Limiter
	ctx context.Context
}

func (r *reader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b[:min(len(b), throttleChunk)])
	if werr := r.lm.Wait(r.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

// Writer slows w to the Limiter's byte rate.
func (lm *Limiter) Writer(ctx context.Context, w http.ResponseWriter) http.ResponseWriter {
	if lm.limit.Bytes <= 0 {
		return w
	}
	return &writer{ResponseWriter: w, lm: lm, ctx: ctx}
}

type writer struct {
	http.ResponseWriter
	lm  *Limiter
	ctx context.Context
}

func (w *writer) Write(b []byte) (int, error) {
	var done int
	for len(b) > 0 {
		k := min(len(b), throttleChunk)
		if err := w.lm.Wait(w.ctx, k); err != nil {
			return done, err
		}
		n, err := w.ResponseWriter.Write(b[:k])
		done += n
		if err != nil {
			return done, err
		}
		b = b[k:]
	}
	return done, nil
}

// Unwrap lets http.ResponseController reach the writer below.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Set keeps one Limiter per client name (anonymous requests by address), creating them on
// first use; once full it drops those idle longer than idle. Past max clients the newcomers
// share one Limiter, so spreading requests over many addresses buys no more than one.
type Set struct {
	limit    Limit
	idle     time.Duration
	max      int
	now      func() time.Time
	overflow *Limiter

	mu     sync.Mutex
	byKey  map[string]*Limiter
	pruned time.Time
}

// NewSet returns a Set of limiters for l, keeping at most max of them.
func NewSet(l Limit, idle time.Duration, max int) *Set {
	return &Set{limit: l, idle: idle, max: max, now: time.Now, overflow: New(l), byKey: map[string]*Limiter{}}
}

// pruneEvery bounds how often a full Set scans for idle clients, which holds its lock.
const pruneEvery = time.Second

func (lm *Limiter) touch(now time.Time) {
	lm.mu.Lock()
	lm.seen = now
	lm.mu.Unlock()
}

// Get returns the Limiter for key.
func (s *Set) Get(key string) *Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if lm, ok := s.byKey[key]; ok {
		lm.touch(now) // so a prune before its request does not drop it
		return lm
	}
	if len(s.byKey) >= s.max {
		if now.Sub(s.pruned) >= pruneEvery {
			s.pruned = now
			s.prune()
		}
		if len(s.byKey) >= s.max {
			return s.overflow
		}
	}
	lm := newAt(s.limit, s.now)
	s.byKey[key] = lm
	return lm
}

func (s *Set) prune() {
	cutoff := s.now().Add(-s.idle)
	for k, lm := range s.byKey {
		lm.mu.Lock()
		idle := lm.seen.Before(cutoff)
		lm.mu.Unlock()
		if idle {
			delete(s.byKey, k)
		}
	}
}
