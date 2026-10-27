package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// ErrProbeTimeout reports a root that did not answer in time, such as a hung network mount.
var ErrProbeTimeout = errors.New("the storage root did not answer in time")

// ErrRootMoved reports that the root's path no longer leads to the directory the store
// opened. Unlike a slow or failing filesystem it does not pass: only reopening the root,
// that is restarting, follows the path again.
var ErrRootMoved = errors.New("the storage root was removed, replaced or unmounted")

type probe struct {
	done chan struct{}
	free int64
	err  error
}

// Probe reads the root directory, checks that its path still leads there and measures
// the free space, failing when any of it fails or takes longer than timeout. free is -1
// where the platform cannot measure it. Calls made while a probe is pending wait for it,
// so a hung mount holds one goroutine, not one per call.
func (s *Store) Probe(timeout time.Duration) (free int64, err error) {
	s.probeMu.Lock()
	p := s.probing
	if p == nil {
		p = &probe{done: make(chan struct{})}
		s.probing = p
		go func() {
			p.free, p.err = s.probeRead()
			s.probeMu.Lock()
			s.probing = nil
			s.probeMu.Unlock()
			close(p.done)
		}()
	}
	s.probeMu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return p.free, p.err
	case <-t.C:
		return -1, ErrProbeTimeout
	}
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
