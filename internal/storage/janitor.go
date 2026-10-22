package storage

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleTmpAge is how long an abandoned staging file is kept; uploads in flight finish well before.
const staleTmpAge = 24 * time.Hour

// RunJanitor measures usage, then calls Sweep and MeasureUsage every interval until ctx
// is cancelled.
func (s *Store) RunJanitor(ctx context.Context, interval, uploadExpiry time.Duration, log *slog.Logger) {
	s.MeasureUsage(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.Sweep(ctx, uploadExpiry); n > 0 {
				log.Info("janitor removed stale entries", "count", n)
			}
			s.MeasureUsage(ctx)
		}
	}
}

// Sweep removes stale staging files, multipart uploads older than uploadExpiry, and
// metadata whose object or bucket is gone. It returns how many entries it removed and
// stops early when ctx is cancelled.
func (s *Store) Sweep(ctx context.Context, uploadExpiry time.Duration) int {
	now := s.now()
	removed := 0
	rm := func(path string) {
		if os.RemoveAll(path) == nil {
			removed++
		}
	}

	if ents, err := os.ReadDir(s.sys("tmp")); err == nil {
		for _, e := range ents {
			if ctx.Err() != nil {
				return removed
			}
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > staleTmpAge {
				rm(s.sys("tmp", e.Name()))
			}
		}
	}

	if ents, err := os.ReadDir(s.sys("uploads")); err == nil {
		for _, e := range ents {
			if ctx.Err() != nil {
				return removed
			}
			dir := s.sys("uploads", e.Name())
			var u uploadMeta
			raw, err := os.ReadFile(filepath.Join(dir, uploadMetaFile))
			switch {
			case err == nil && json.Unmarshal(raw, &u) == nil:
				if now.Sub(u.Initiated) > uploadExpiry {
					rm(dir)
				}
			default:
				// No readable manifest: a part landed after an abort recreated the directory, or it is corrupt.
				if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > staleTmpAge {
					rm(dir)
				}
			}
		}
	}

	if ents, err := os.ReadDir(s.sys("meta")); err == nil {
		for _, e := range ents {
			if ctx.Err() != nil {
				return removed
			}
			if s.sweepGoneBucket(e.Name()) {
				removed++
				continue
			}
			removed += s.sweepSidecars(ctx, e.Name())
		}
	}

	if ents, err := os.ReadDir(s.sys("buckets")); err == nil {
		for _, e := range ents {
			if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && s.requireBucket(name) != nil {
				rm(s.sys("buckets", e.Name()))
			}
		}
	}
	return removed
}

// sweepGoneBucket removes the metadata of a bucket that no longer exists, holding the
// bucket lock so a bucket recreated meanwhile keeps its new sidecars.
func (s *Store) sweepGoneBucket(bucket string) bool {
	bl := s.bucketLock(bucket)
	bl.Lock()
	defer bl.Unlock()
	if s.requireBucket(bucket) == nil {
		return false
	}
	return os.RemoveAll(s.sys("meta", bucket)) == nil
}

// sweepSidecars removes the sidecars of bucket whose data file or directory marker no
// longer exists.
func (s *Store) sweepSidecars(ctx context.Context, bucket string) int {
	removed := 0
	_ = filepath.WalkDir(s.sys("meta", bucket), func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil || d.IsDir() {
			return nil
		}
		var m sidecar
		raw, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, &m) != nil || checkKey(m.Key) != nil {
			if os.Remove(path) == nil {
				removed++
			}
			return nil
		}
		unlock := s.lock(bucket, m.Key)
		defer unlock()
		fi, err := s.r.Stat(s.rel(s.dataPath(bucket, m.Key)))
		if err != nil || fi.IsDir() != m.Dir || (!m.Dir && !fi.Mode().IsRegular()) {
			if os.Remove(path) == nil {
				removed++
			}
		}
		return nil
	})
	return removed
}

// Usage is the stored object count and size as of the last measurement.
type Usage struct {
	Objects int64
	Bytes   int64
	At      time.Time // zero until the first measurement
}

// Usage returns the last measurement taken by MeasureUsage.
func (s *Store) Usage() Usage {
	if u := s.usage.Load(); u != nil {
		return *u
	}
	return Usage{}
}

// MeasureUsage counts the regular files in every bucket. It walks the whole tree, so the
// janitor runs it rather than each metrics scrape.
func (s *Store) MeasureUsage(ctx context.Context) {
	var u Usage
	buckets, err := s.ListBuckets()
	if err != nil {
		return
	}
	for _, b := range buckets {
		_ = filepath.WalkDir(s.bucketPath(b.Name), func(_ string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			if fi, err := d.Info(); err == nil {
				u.Objects++
				u.Bytes += fi.Size()
			}
			return nil
		})
	}
	if ctx.Err() != nil {
		return
	}
	u.At = s.now()
	s.usage.Store(&u)
}
