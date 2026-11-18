package storage

import (
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// ErrInsufficientStorage refuses a write that would leave less free space on the root's
// filesystem than SetMinFree asks for.
var ErrInsufficientStorage = errors.New("free space on the root's filesystem is below the configured minimum")

// errNoDiskSpace reports a platform without a free-space query; the check is then off.
var errNoDiskSpace = errors.New("free space not available on this platform")

// SetMinFree sets the free bytes every write must leave on the root's filesystem; 0
// turns the check off. Safe to call while serving.
func (s *Store) SetMinFree(n int64) { s.minFree.Store(n) }

// MinFree returns the minimum set by SetMinFree.
func (s *Store) MinFree() int64 { return s.minFree.Load() }

// DiskSpace returns the bytes an unprivileged writer may still use on the root's
// filesystem and the filesystem's size.
func (s *Store) DiskSpace() (free, size int64, err error) { return s.statfs(s.root) }

type spaceReading struct{ free, size int64 }

// DiskSpaceWithin is DiskSpace failing with ErrProbeTimeout after timeout, for callers
// that must answer while a network mount hangs. Calls made meanwhile share one query.
func (s *Store) DiskSpaceWithin(timeout time.Duration) (free, size int64, err error) {
	d, err := s.spaces.wait(timeout, func() (spaceReading, error) {
		free, size, err := s.statfs(s.root)
		return spaceReading{free, size}, err
	})
	return d.free, d.size, err
}

// reserve admits a write of size more bytes (negative: unknown) and holds them against
// other writes until release. Bytes a write in progress has already stored are counted
// twice, so concurrent writes are refused a little early, never late. budget is what
// all writes in progress may hold together, or -1 without a minimum. A filesystem that
// cannot be queried is not held against the write.
func (s *Store) reserve(size int64) (release func(), budget int64, err error) {
	size = max(size, 0)
	release = func() { s.pending.Add(-size) }
	limit := s.minFree.Load()
	if limit <= 0 {
		s.pending.Add(size)
		return release, -1, nil
	}
	free, _, err := s.statfs(s.root)
	if err != nil {
		s.pending.Add(size)
		return release, -1, nil
	}
	budget = free - limit
	// A refused write never touches pending, so a body counting against it is not cut by one.
	for {
		p := s.pending.Load()
		if p+size > budget {
			return nil, 0, ErrInsufficientStorage
		}
		if s.pending.CompareAndSwap(p, p+size) {
			return release, budget, nil
		}
	}
}

// roomReader fails a body of unknown size once what all writes in progress hold, its own
// bytes counted into pending as they arrive, passes budget.
type roomReader struct {
	r       io.Reader
	budget  int64
	pending *atomic.Int64
	held    int64
}

func (r *roomReader) Read(p []byte) (int, error) {
	left := r.budget - r.pending.Load()
	if left < 0 {
		return 0, ErrInsufficientStorage
	}
	// One byte past what is left is enough to tell the body does not fit.
	if int64(len(p)) > left+1 {
		p = p[:left+1]
	}
	n, err := r.r.Read(p)
	r.held += int64(n)
	if r.pending.Add(int64(n)) > r.budget {
		return n, ErrInsufficientStorage
	}
	return n, err
}

// release gives back what the body held.
func (r *roomReader) release() { r.pending.Add(-r.held) }
