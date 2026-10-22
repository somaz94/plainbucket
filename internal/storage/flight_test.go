package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitDups blocks until n callers are waiting on the running call for key.
func waitDups(t *testing.T, g *flight, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		c := g.calls[key]
		got := -1
		if c != nil {
			got = c.dups
		}
		g.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFlightSharesResult(t *testing.T) {
	var g flight
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func() ([]byte, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("boom")
	}
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(1)
	go func() { defer wg.Done(); _, errs[0] = g.do("k", fn) }()
	waitDups(t, &g, "k", 0)
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = g.do("k", fn) }()
	}
	waitDups(t, &g, "k", n-1)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("fn ran %d times, want 1", calls.Load())
	}
	for i, err := range errs {
		if err == nil || err.Error() != "boom" {
			t.Errorf("caller %d got %v", i, err)
		}
	}
	if len(g.calls) != 0 {
		t.Errorf("calls left behind: %v", g.calls)
	}
	// A later call runs again.
	if _, err := g.do("k", func() ([]byte, error) { calls.Add(1); return nil, nil }); err != nil || calls.Load() != 2 {
		t.Errorf("second call: %v, %d runs", err, calls.Load())
	}
}

// Concurrent HEADs of a file copied in from outside hash it once.
func TestConcurrentHeadsHashOnce(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(filepath.Join(s.root, "bkt", "big"), []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var calls atomic.Int32
	orig := hashFile
	hashFile = func(r io.Reader) ([]byte, error) {
		calls.Add(1)
		<-release
		return orig(r)
	}
	t.Cleanup(func() { hashFile = orig })

	fi, err := os.Stat(filepath.Join(s.root, "bkt", "big"))
	if err != nil {
		t.Fatal(err)
	}
	id := "bkt/big\x00" + "8" + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	const n = 4
	etags := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := s.HeadObject("bkt", "big")
			if err != nil {
				t.Error(err)
			}
			etags[i] = info.ETag
		}()
	}
	waitDups(t, &s.hashes, id, n-1)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("hashed %d times, want 1", calls.Load())
	}
	for _, e := range etags {
		if e != md5ETag(md5sum("external")) {
			t.Errorf("etag = %s", e)
		}
	}
}

func TestCopyObjectCanceled(t *testing.T) {
	s := newStore(t)
	put(t, s, "src", strings.Repeat("x", 1<<16))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CopyObject(ctx, "bkt", "src", "bkt", "dst", CopyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := s.HeadObject("bkt", "dst"); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("destination exists after a canceled copy: %v", err)
	}
	if left, _ := os.ReadDir(s.sys("tmp")); len(left) != 0 {
		t.Errorf("staged files left: %v", left)
	}
}

func TestFlightPanicFailsWaiters(t *testing.T) {
	var g flight
	release := make(chan struct{})
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_, _ = g.do("k", func() ([]byte, error) { <-release; panic("boom") })
	}()
	waitDups(t, &g, "k", 0)
	done := make(chan error, 1)
	go func() { _, err := g.do("k", func() ([]byte, error) { return []byte("x"), nil }); done <- err }()
	waitDups(t, &g, "k", 1)
	close(release)
	if err := <-done; !errors.Is(err, errFlightPanicked) {
		t.Errorf("waiter got %v, want errFlightPanicked", err)
	}
	if p := <-leader; p != "boom" {
		t.Errorf("leader panic = %v", p)
	}
}
