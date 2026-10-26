package ratelimit

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestAllow(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	lm := newAt(Limit{Requests: 2, Burst: 3}, c.now)
	for i := range 3 {
		if ok, _ := lm.Allow(); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := lm.Allow()
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("past the burst: %v %v", ok, wait)
	}
	c.t = c.t.Add(500 * time.Millisecond)
	if ok, _ := lm.Allow(); !ok {
		t.Error("a refilled token refused")
	}
	// The bucket never holds more than the burst.
	c.t = c.t.Add(time.Hour)
	for range 3 {
		lm.Allow()
	}
	if ok, _ := lm.Allow(); ok {
		t.Error("an idle hour bought more than the burst")
	}
	if ok, _ := newAt(Limit{Bytes: 10}, c.now).Allow(); !ok {
		t.Error("no request limit refused a request")
	}
	if b := (Limit{Requests: 2.5}).burst(); b != 3 {
		t.Errorf("default burst %v", b)
	}
	if !(Limit{}).Zero() || (Limit{Bytes: 1}).Zero() {
		t.Error("Zero")
	}
}

func TestWaitAndThrottle(t *testing.T) {
	lm := New(Limit{Bytes: 100 << 10}) // 100 KiB/s
	ctx := context.Background()
	start := time.Now()
	r := lm.Reader(ctx, io.NopCloser(bytes.NewReader(make([]byte, 20<<10))))
	if n, _ := io.Copy(io.Discard, r); n != 20<<10 {
		t.Fatalf("read %d", n)
	}
	rec := httptest.NewRecorder()
	w := lm.Writer(ctx, rec)
	if n, err := w.Write(make([]byte, 20<<10)); n != 20<<10 || err != nil {
		t.Fatalf("wrote %d %v", n, err)
	}
	// 40 KiB at 100 KiB/s, shared by the reader and the writer.
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Errorf("40 KiB took %v", d)
	}
	if w.(interface{ Unwrap() http.ResponseWriter }).Unwrap() != rec {
		t.Error("Unwrap does not reach the writer below")
	}
}

func TestWaitCanceled(t *testing.T) {
	lm := New(Limit{Bytes: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lm.Wait(ctx, 10); err == nil {
		t.Error("a canceled wait returned no error")
	}
	w := lm.Writer(ctx, httptest.NewRecorder())
	if n, err := w.Write([]byte("abc")); n != 0 || err == nil {
		t.Errorf("canceled write: %d %v", n, err)
	}
	r := lm.Reader(ctx, io.NopCloser(strings.NewReader("abc")))
	if _, err := r.Read(make([]byte, 3)); err == nil {
		t.Error("canceled read returned no error")
	}
}

func TestUnlimitedPassesThrough(t *testing.T) {
	lm := New(Limit{Requests: 5})
	body := io.NopCloser(strings.NewReader("x"))
	if lm.Reader(context.Background(), body) != body {
		t.Error("reader wrapped without a byte limit")
	}
	rec := httptest.NewRecorder()
	if lm.Writer(context.Background(), rec) != rec {
		t.Error("writer wrapped without a byte limit")
	}
	if err := lm.Wait(context.Background(), 1<<30); err != nil {
		t.Error(err)
	}
}

func TestSet(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := NewSet(Limit{Requests: 1}, time.Minute, 2)
	s.now = c.now
	a := s.Get("a")
	if s.Get("a") != a {
		t.Fatal("a client got two limiters")
	}
	b := s.Get("b")
	// Full of busy clients: a newcomer shares the overflow limiter.
	if got := s.Get("c"); got != s.overflow || got == a || got == b {
		t.Error("third client not sent to the overflow limiter")
	}
	// Once one goes idle it makes room.
	c.t = c.t.Add(2 * time.Minute)
	s.Get("b").Allow()
	if got := s.Get("d"); got == s.overflow {
		t.Error("no room made by an idle client")
	}
	if _, ok := s.byKey["a"]; ok {
		t.Error("idle client kept")
	}
}

func TestCanceledWaitRefunds(t *testing.T) {
	lm := New(Limit{Bytes: 1000})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lm.Wait(ctx, 10000); err == nil { // 10s of debt, given up after 20ms
		t.Fatal("a wait past its deadline returned no error")
	}
	start := time.Now()
	if err := lm.Wait(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the next 50 bytes waited %v: the abandoned debt stayed", d)
	}
}

func TestSetPrunesAtMostOncePerInterval(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := NewSet(Limit{Requests: 1}, time.Minute, 1)
	s.now = c.now
	s.Get("a")
	c.t = c.t.Add(2 * time.Minute) // a is idle now
	if s.Get("b") == s.overflow {
		t.Fatal("first newcomer not given a's place")
	}
	c.t = c.t.Add(2 * time.Minute) // b idle too, but the last prune was moments ago in Set time
	s.pruned = c.t
	if s.Get("c") != s.overflow {
		t.Error("pruned again within the interval")
	}
	// A hit refreshes the client's last use, so it is not taken for idle.
	c.t = c.t.Add(pruneEvery)
	s.Get("b")
	if s.Get("d") != s.overflow {
		t.Error("a client just seen was pruned")
	}
}
