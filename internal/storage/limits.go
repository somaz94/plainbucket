package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ErrQuotaExceeded rejects a write that would take a bucket past its size limit.
var ErrQuotaExceeded = errors.New("bucket size limit reached")

// touchInterval bounds how often a read records itself as the object's last use.
const touchInterval = time.Hour

// Limit caps one bucket.
type Limit struct {
	// ExpireAfter removes objects neither written nor read for this long; 0 keeps them.
	ExpireAfter time.Duration
	// MaxSize caps the bucket's bytes; 0 is unlimited.
	MaxSize int64
	// Evict makes MaxSize evict the least recently used objects instead of refusing writes.
	Evict bool
}

func (l Limit) evicts() bool { return l.Evict && l.MaxSize > 0 }

// SetLimits sets the per-bucket limits. Call it before serving.
func (s *Store) SetLimits(limits map[string]Limit) { s.limits = limits }

// fits rejects early, before the body is received, a write of size bytes to bucket/key
// that would pass a refusing limit. commit checks again with the real figures.
func (s *Store) fits(bucket, key string, size int64) error {
	l := s.limits[bucket]
	if l.MaxSize == 0 || l.Evict {
		return nil
	}
	if fi, err := s.r.Lstat(s.rel(s.dataPath(bucket, key))); err == nil && fi.Mode().IsRegular() {
		size -= fi.Size()
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.bucketUsage[bucket].Bytes+size > l.MaxSize {
		return ErrQuotaExceeded
	}
	return nil
}

// account adds a change to bucket's usage. A growth that passes a refusing limit is
// rejected with nothing recorded. over reports an evicting bucket now past its limit.
func (s *Store) account(bucket string, objects, bytes int64) (over bool, err error) {
	l := s.limits[bucket]
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	u := s.bucketUsage[bucket]
	if bytes > 0 && l.MaxSize > 0 && !l.Evict && u.Bytes+bytes > l.MaxSize {
		return false, ErrQuotaExceeded
	}
	u.Objects += objects
	u.Bytes += bytes
	s.bucketUsage[bucket] = u
	return l.evicts() && u.Bytes > l.MaxSize, nil
}

// wake asks the janitor to apply the limits without waiting for its next round.
func (s *Store) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// BucketUsage returns the tracked usage of the buckets that have limits. Writes and
// deletes update it as they happen; the janitor re-measures it hourly.
func (s *Store) BucketUsage() map[string]Usage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	out := map[string]Usage{}
	for name := range s.limits {
		out[name] = s.bucketUsage[name]
	}
	return out
}

// Removed returns how many objects expiry and eviction have deleted since startup.
func (s *Store) Removed() (expired, evicted int64) { return s.expired.Load(), s.evicted.Load() }

// touch records a read as the object's last use by moving the sidecar's mtime, at most
// once per touchInterval. The sidecar is trusted by the size and mtime it records, not
// by its own mtime, so this does not invalidate it.
func (s *Store) touch(bucket, key string) {
	p := s.metaPath(bucket, key)
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	if now := s.now(); now.Sub(fi.ModTime()) >= touchInterval {
		_ = os.Chtimes(p, now, now)
	}
}

// lastUse is the later of the data file's mtime (written) and its sidecar's (read).
func (s *Store) lastUse(bucket, key string, data os.FileInfo) time.Time {
	t := data.ModTime()
	if fi, err := os.Stat(s.metaPath(bucket, key)); err == nil && fi.ModTime().After(t) {
		t = fi.ModTime()
	}
	return t
}

type candidate struct {
	key     string
	size    int64
	mtimeNs int64
	used    time.Time
}

// enforceLimits expires and evicts objects in the buckets that have limits.
func (s *Store) enforceLimits(ctx context.Context) (expired, evicted int) {
	for bucket, l := range s.limits {
		if ctx.Err() != nil {
			return
		}
		if l.ExpireAfter == 0 && !l.evicts() || s.requireBucket(bucket) != nil {
			continue
		}
		objs := s.scanBucket(ctx, bucket)
		if l.ExpireAfter > 0 {
			cutoff := s.now().Add(-l.ExpireAfter)
			objs = slices.DeleteFunc(objs, func(c candidate) bool {
				if c.used.Before(cutoff) && s.removeIfUnused(bucket, c, cutoff) {
					expired++
					return true
				}
				return false
			})
		}
		if l.evicts() {
			var total int64
			for _, c := range objs {
				total += c.size
			}
			slices.SortFunc(objs, func(a, b candidate) int { return a.used.Compare(b.used) })
			for _, c := range objs {
				if total <= l.MaxSize || ctx.Err() != nil {
					break
				}
				// An object rewritten since the scan stays; the next pass sees its new size.
				if s.removeIfUnused(bucket, c, time.Time{}) {
					total -= c.size
					evicted++
				}
			}
		}
	}
	s.expired.Add(int64(expired))
	s.evicted.Add(int64(evicted))
	return expired, evicted
}

// scanBucket lists the regular files of bucket with their last use.
func (s *Store) scanBucket(ctx context.Context, bucket string) []candidate {
	var out []candidate
	root := s.bucketPath(bucket)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		key := filepath.ToSlash(rel)
		fi, ierr := d.Info()
		if err != nil || ierr != nil || checkKey(key) != nil {
			return nil
		}
		out = append(out, candidate{key: key, size: fi.Size(), mtimeNs: fi.ModTime().UnixNano(), used: s.lastUse(bucket, key, fi)})
		return nil
	})
	return out
}

// removeIfUnused deletes c unless it changed since the scan or, with a non-zero
// usedBefore, was used at or after it.
func (s *Store) removeIfUnused(bucket string, c candidate, usedBefore time.Time) bool {
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	unlock := s.lock(bucket, c.key)
	defer unlock()
	p := s.dataPath(bucket, c.key)
	fi, err := s.r.Lstat(s.rel(p))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != c.size || fi.ModTime().UnixNano() != c.mtimeNs {
		return false
	}
	if !usedBefore.IsZero() && !s.lastUse(bucket, c.key, fi).Before(usedBefore) {
		return false
	}
	return s.removeFile(bucket, c.key, p, fi) == nil
}
