package storage

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// Rules override ExpireAfter for the keys under their prefix; the longest prefix wins.
	Rules []LifecycleRule
}

// LifecycleRule expires the objects under Prefix: ExpireAfter counts from the last use,
// DeleteAfter from the last write. A rule with neither exempts its prefix from expiry,
// though not from eviction: a bucket over an evicting MaxSize must shrink regardless.
type LifecycleRule struct {
	Prefix      string
	ExpireAfter time.Duration
	DeleteAfter time.Duration
}

func (l Limit) evicts() bool { return l.Evict && l.MaxSize > 0 }

func (l Limit) expires() bool {
	return l.ExpireAfter > 0 || slices.ContainsFunc(l.Rules, func(r LifecycleRule) bool { return r.ExpireAfter > 0 || r.DeleteAfter > 0 })
}

// ruleFor returns the expiry of the longest rule prefix matching key, else the bucket's.
func (l Limit) ruleFor(key string) (expireAfter, deleteAfter time.Duration) {
	best := -1
	for i, r := range l.Rules {
		if strings.HasPrefix(key, r.Prefix) && (best < 0 || len(r.Prefix) > len(l.Rules[best].Prefix)) {
			best = i
		}
	}
	if best < 0 {
		return l.ExpireAfter, 0
	}
	return l.Rules[best].ExpireAfter, l.Rules[best].DeleteAfter
}

// SetLimits sets the per-bucket limits. Call it before serving.
func (s *Store) SetLimits(limits map[string]Limit) { s.limits.Store(&limits) }

// limitsNow returns the limits in force; the map is never modified once set.
func (s *Store) limitsNow() map[string]Limit {
	if m := s.limits.Load(); m != nil {
		return *m
	}
	return nil
}

// fits rejects early, before the body is received, a write of size bytes to bucket/key
// that would pass a refusing limit. commit checks again with the real figures.
func (s *Store) fits(bucket, key string, size int64) error {
	l := s.limitsNow()[bucket]
	if l.MaxSize == 0 {
		return nil
	}
	if l.Evict {
		// Accepted, it would evict everything else and then itself.
		if size > l.MaxSize {
			return ErrQuotaExceeded
		}
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
	l := s.limitsNow()[bucket]
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	u := s.bucketUsage[bucket]
	if bytes > 0 && l.MaxSize > 0 && !l.Evict && u.Bytes+bytes > l.MaxSize {
		return false, ErrQuotaExceeded
	}
	s.addUsage(bucket, objects, bytes)
	return l.evicts() && s.bucketUsage[bucket].Bytes > l.MaxSize, nil
}

// adjust records a change that no limit may refuse: a delete, or undoing an account.
func (s *Store) adjust(bucket string, objects, bytes int64) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.addUsage(bucket, objects, bytes)
}

// resetUsage records that bucket was just created empty (exists) or deleted, so a running
// measurement keeps only the changes made after it. The caller holds usageMu.
func (s *Store) resetUsage(bucket string, exists bool) {
	if exists {
		s.bucketUsage[bucket] = Usage{}
	} else {
		delete(s.bucketUsage, bucket)
	}
	if s.measuring != nil {
		s.measuring[bucket] = Usage{}
		s.resets[bucket] = exists
	}
}

// addUsage applies a change, and keeps it for replay if a measurement is running, whose
// result would otherwise overwrite it. The caller holds usageMu.
func (s *Store) addUsage(bucket string, objects, bytes int64) {
	u := s.bucketUsage[bucket]
	u.Objects += objects
	u.Bytes += bytes
	s.bucketUsage[bucket] = u
	if s.measuring != nil {
		d := s.measuring[bucket]
		d.Objects += objects
		d.Bytes += bytes
		s.measuring[bucket] = d
	}
}

// wake asks the janitor to apply the limits without waiting for its next round.
func (s *Store) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// BucketUsage returns the tracked usage of the buckets that have limits. Writes and
// deletes update it as they happen; each janitor round re-measures it.
func (s *Store) BucketUsage() map[string]Usage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	out := map[string]Usage{}
	for name := range s.limitsNow() {
		out[name] = s.bucketUsage[name]
	}
	return out
}

// AllBucketUsage returns every bucket's tracked usage. Writes and deletes update it as
// they happen; each janitor round re-measures it.
func (s *Store) AllBucketUsage() map[string]Usage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return maps.Clone(s.bucketUsage)
}

// BucketLimit returns the limits set for bucket.
func (s *Store) BucketLimit(bucket string) Limit { return s.limitsNow()[bucket] }

// Limits returns a copy of every bucket's limits, all from one SetLimits.
func (s *Store) Limits() map[string]Limit { return maps.Clone(s.limitsNow()) }

// Removed returns how many objects expiry and eviction have deleted since startup.
func (s *Store) Removed() (expired, evicted int64) { return s.expired.Load(), s.evicted.Load() }

// touch records a read as the object's last use by moving the sidecar's mtime, at most
// once per touchInterval. The sidecar is trusted by the size and mtime it records, not
// by its own mtime, so this does not invalidate it.
func (s *Store) touch(bucket, key string) {
	every := touchInterval
	// A short expiry needs finer records, or an object read all along would expire.
	if e, _ := s.limitsNow()[bucket].ruleFor(key); e > 0 && e/4 < every {
		every = e / 4
	}
	p := s.metaPath(bucket, key)
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	if now := s.now(); now.Sub(fi.ModTime()) >= every {
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
	mtimeNs int64     // identity, compared exactly
	written time.Time // for delete-after; UnixNano wraps outside 1678 to 2262
	used    time.Time
}

// OnLimitRemove sets fn to be called, outside the store's locks, for every object that
// expiry or eviction removes.
func (s *Store) OnLimitRemove(fn func(bucket, key string)) { s.onLimit.Store(&fn) }

func (s *Store) limitRemoved(bucket, key string) {
	if fn := s.onLimit.Load(); fn != nil {
		(*fn)(bucket, key)
	}
}

// enforceLimits expires and evicts objects in the buckets that have limits.
func (s *Store) enforceLimits(ctx context.Context) (expired, evicted int) {
	for bucket, l := range s.limitsNow() {
		if ctx.Err() != nil {
			return
		}
		if (!l.expires() && !l.evicts()) || s.requireBucket(bucket) != nil {
			continue
		}
		objs := s.scanBucket(ctx, bucket)
		if l.expires() {
			now := s.now()
			objs = slices.DeleteFunc(objs, func(c candidate) bool {
				expireAfter, deleteAfter := l.ruleFor(c.key)
				var removed bool
				switch {
				case deleteAfter > 0 && c.written.Before(now.Add(-deleteAfter)):
					removed = s.removeUnchanged(bucket, c, nil) // however recently read
				case expireAfter > 0 && c.used.Before(now.Add(-expireAfter)):
					removed = s.removeIfUnused(bucket, c, now.Add(-expireAfter))
				}
				if removed {
					expired++
					s.limitRemoved(bucket, c.key)
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
				// An object rewritten or read since the scan stays; the next pass sees it again.
				if s.removeIfUnused(bucket, c, c.used.Add(time.Nanosecond)) {
					total -= c.size
					evicted++
					s.limitRemoved(bucket, c.key)
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
		out = append(out, candidate{key: key, size: fi.Size(), mtimeNs: fi.ModTime().UnixNano(), written: fi.ModTime(), used: s.lastUse(bucket, key, fi)})
		return nil
	})
	return out
}

// removeIfUnused deletes c unless it changed since the scan or was used at or after
// usedBefore.
func (s *Store) removeIfUnused(bucket string, c candidate, usedBefore time.Time) bool {
	return s.removeUnchanged(bucket, c, func(fi os.FileInfo) bool { return s.lastUse(bucket, c.key, fi).Before(usedBefore) })
}

// removeUnchanged deletes c unless it changed since the scan or ok, given the file found
// under the key lock, refuses; a nil ok checks nothing more.
func (s *Store) removeUnchanged(bucket string, c candidate, ok func(os.FileInfo) bool) bool {
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	unlock := s.lock(bucket, c.key)
	defer unlock()
	if s.paused.Load() {
		return false // paused while a pass was under way
	}
	p := s.dataPath(bucket, c.key)
	fi, err := s.r.Lstat(s.rel(p))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != c.size || fi.ModTime().UnixNano() != c.mtimeNs {
		return false
	}
	if ok != nil && !ok(fi) {
		return false
	}
	removed, err := s.removeFile(bucket, c.key, p, fi)
	return removed && err == nil
}
