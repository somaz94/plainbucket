package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// ErrProbeTimeout reports a root or filesystem query that did not answer in time, such as a
// hung network mount.
var ErrProbeTimeout = errors.New("the storage root did not answer in time")

// ErrRootMoved reports that the root's path no longer leads to the directory the store
// opened. Unlike a slow or failing filesystem it does not pass: only reopening the root,
// that is restarting, follows the path again.
var ErrRootMoved = errors.New("the storage root was removed, replaced or unmounted")

// shared runs one call at a time and lets each caller wait for it at most a timeout, so a
// hung mount holds one goroutine, not one per caller.
type shared[T any] struct {
	mu   sync.Mutex
	call *sharedCall[T]
}

type sharedCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func (g *shared[T]) wait(timeout time.Duration, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	c := g.call
	if c == nil {
		c = &sharedCall[T]{done: make(chan struct{})}
		g.call = c
		go func() {
			c.val, c.err = fn()
			g.mu.Lock()
			g.call = nil
			g.mu.Unlock()
			close(c.done)
		}()
	}
	g.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.done:
		return c.val, c.err
	case <-t.C:
		var zero T
		return zero, ErrProbeTimeout
	}
}

// Probe reads the root directory, checks that its path still leads there and measures
// the free space, failing when any of it fails or takes longer than timeout. free is -1
// where the platform cannot measure it or on timeout; concurrent calls share one probe.
func (s *Store) Probe(timeout time.Duration) (free int64, err error) {
	free, err = s.probes.wait(timeout, s.probeRead)
	if errors.Is(err, ErrProbeTimeout) {
		return -1, err
	}
	return free, err
}

func (s *Store) readRoot() (int64, error) {
	f, err := s.r.Open(".")
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return -1, err
	}
	opened, err := f.Stat()
	if err != nil {
		return -1, err
	}
	named, err := os.Stat(s.root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return -1, fmt.Errorf("%w: %w", ErrRootMoved, err)
	case err != nil:
		return -1, err
	case !os.SameFile(opened, named):
		return -1, ErrRootMoved
	}
	// statfs also reaches a network filesystem's server, which a cached directory read may not.
	free, _, err := s.statfs(s.root)
	if errors.Is(err, errNoDiskSpace) {
		return -1, nil
	}
	return free, err
}
