package storage

import (
	"errors"
	"io"
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
// twice, so concurrent writes are refused a little early, never late. room is what the
// write may store at most, or -1 without a minimum. A filesystem that cannot be queried
// is not held against the write.
func (s *Store) reserve(size int64) (release func(), room int64, err error) {
	size = max(size, 0)
	pending := s.pending.Add(size)
	release = func() { s.pending.Add(-size) }
	limit := s.minFree.Load()
	if limit <= 0 {
		return release, -1, nil
	}
	free, _, err := s.statfs(s.root)
	if err != nil {
		return release, -1, nil
	}
	room = free - pending - limit
	if room < 0 {
		release()
		return nil, 0, ErrInsufficientStorage
	}
	return release, room + size, nil
}

// roomReader fails a body of unknown size once it passes n bytes.
type roomReader struct {
	r io.Reader
	n int64
}

func (r *roomReader) Read(p []byte) (int, error) {
	// One byte past n is enough to tell the body does not fit.
	if int64(len(p)) > r.n+1 {
		p = p[:r.n+1]
	}
	n, err := r.r.Read(p)
	if r.n -= int64(n); r.n < 0 {
		return n, ErrInsufficientStorage
	}
	return n, err
}
