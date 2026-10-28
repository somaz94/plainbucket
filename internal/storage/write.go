package storage

import (
	"crypto/md5" //nolint:gosec // S3 ETag format, not a security boundary
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Conditions are the If-Match / If-None-Match headers of a write.
type Conditions struct {
	IfMatch     string // ETag the current object must have
	IfNoneMatch string // "*": the key must not exist; an ETag: the current object must not have it
}

// Verifier checks the received body before it replaces anything. md5 is the body's
// digest; it returns the verified additional checksums to store with the object.
type Verifier func(md5 []byte) (map[string]string, error)

// receive streams body (exactly size bytes when size >= 0) into a temporary file and
// returns its path, MD5 and verified checksums. The caller removes the file.
func (s *Store) receive(body io.Reader, size int64, verify Verifier) (string, []byte, map[string]string, error) {
	release, room, err := s.reserve(size)
	if err != nil {
		return "", nil, nil, err
	}
	defer release()
	if size < 0 && room >= 0 {
		body = &roomReader{r: body, n: room}
	}
	tmp, err := os.CreateTemp(s.sys("tmp"), "upload-*")
	if err != nil {
		return "", nil, nil, err
	}
	name := tmp.Name()
	h := md5.New() //nolint:gosec // S3 ETag format
	if size >= 0 {
		body = io.LimitReader(body, size)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err == nil && size >= 0 && n != size {
		err = ErrShortBody
	}
	sum := h.Sum(nil)
	var checks map[string]string
	if err == nil && verify != nil {
		checks, err = verify(sum)
	}
	if err == nil {
		err = tmp.Chmod(filePerm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", nil, nil, err
	}
	return name, sum, checks, nil
}

// commit moves the received file tmp into place as bucket/key under the key lock,
// after checking cond against the current object.
func (s *Store) commit(bucket, key, tmp string, m sidecar, cond Conditions) (ObjectInfo, error) {
	defer func() { _ = os.Remove(tmp) }()
	// Staged before the locks: the sidecar depends only on tmp, and its sync is slow.
	fi, err := os.Stat(tmp)
	if err != nil {
		return ObjectInfo{}, err
	}
	m.Key, m.Size, m.MtimeNs = key, fi.Size(), fi.ModTime().UnixNano()
	metaTmp, err := s.stageSidecar(m)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("writing metadata: %w", err)
	}
	defer func() { _ = os.Remove(metaTmp) }()

	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	unlock := s.lock(bucket, key)
	defer unlock()

	if err := s.requireBucket(bucket); err != nil {
		return ObjectInfo{}, err
	}
	if err := s.checkConditions(bucket, key, cond); err != nil {
		return ObjectInfo{}, err
	}
	dst := s.dataPath(bucket, key)
	var oldObjects, oldBytes int64
	if fi, err := s.r.Lstat(s.rel(dst)); err == nil && fi.IsDir() {
		return ObjectInfo{}, ErrConflict
	} else if err == nil && fi.Mode().IsRegular() {
		oldObjects, oldBytes = 1, fi.Size()
	}

	over, err := s.account(bucket, 1-oldObjects, fi.Size()-oldBytes)
	if err != nil {
		return ObjectInfo{}, err
	}
	committed := false
	defer func() {
		if !committed {
			s.adjust(bucket, oldObjects-1, oldBytes-fi.Size())
		}
	}()
	metaDst := s.metaPath(bucket, key)
	if err := s.place(tmp, dst, dirPerm); err != nil {
		return ObjectInfo{}, err
	}
	committed = true // the data is in place, so the usage change stands
	if over {
		s.wake()
	}
	if err := s.place(metaTmp, metaDst, sysDirPerm); err != nil {
		// With coarse mtimes the old sidecar could still match the new data.
		_ = os.Remove(metaDst)
		return ObjectInfo{}, fmt.Errorf("writing metadata: %w", err)
	}
	return m.info(fi.ModTime()), nil
}

// checkConditions runs with the key lock held.
func (s *Store) checkConditions(bucket, key string, cond Conditions) error {
	if cond.IfMatch == "" && cond.IfNoneMatch == "" {
		return nil
	}
	f, info, err := s.open(bucket, key, false, true)
	if f != nil {
		_ = f.Close()
	}
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNoSuchKey) {
		return err
	}
	if exists && (cond.IfNoneMatch == "*" || cond.IfNoneMatch != "" && trimETag(cond.IfNoneMatch) == trimETag(info.ETag)) {
		return ErrPreconditionFailed
	}
	if cond.IfMatch != "" {
		if !exists {
			return ErrNoSuchKey
		}
		if cond.IfMatch != "*" && trimETag(cond.IfMatch) != trimETag(info.ETag) {
			return ErrPreconditionFailed
		}
	}
	return nil
}

func (s *Store) stageBytes(pattern string, raw []byte) (string, error) {
	tmp, err := os.CreateTemp(s.sys("tmp"), pattern)
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(raw)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// writeFileAtomic replaces path with raw via a staged rename.
func (s *Store) writeFileAtomic(path string, raw []byte) error {
	tmp, err := s.stageBytes("write-*", raw)
	if err != nil {
		return err
	}
	if err := s.place(tmp, path, sysDirPerm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// rename is os.Root.Rename; tests replace it to fail or hold a specific rename.
var rename = func(r *os.Root, src, dst string) error { return r.Rename(src, dst) }

// syncDir flushes a directory, so a rename into or a removal from it survives a power
// loss; tests replace it.
var syncDir = func(r *os.Root, dir string) error {
	d, err := r.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

// ErrSyncUnsupported is wrapped in the error OnSyncError gets once, the first time the
// filesystem refuses to flush a directory, as some network filesystems do. Refusals are
// not counted.
var ErrSyncUnsupported = errors.New("the filesystem does not support flushing directories")

// flushDir syncs dir after a rename into it or a removal from it. The change stands by
// then, so a failure never fails the request: it is counted and reported, a refusal is
// reported once (see ErrSyncUnsupported), and a directory removed since is ignored.
func (s *Store) flushDir(dir string) {
	err := syncDir(s.r, dir)
	fn := s.onSyncError.Load()
	switch {
	case err == nil || errors.Is(err, os.ErrNotExist):
	case errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY):
		// The once is spent only on a listener, so a write before OnSyncError cannot use it up.
		if fn != nil && !s.syncRefused.Swap(true) {
			(*fn)(dir, fmt.Errorf("%w: %w", ErrSyncUnsupported, err))
		}
	default:
		s.syncErrors.Add(1)
		if fn != nil {
			(*fn)(dir, err)
		}
	}
}

// OnSyncError sets fn to be called when a directory flush fails (after a write or a
// delete): the change stands but may not survive a power loss. A refusal
// arrives only once, wrapping ErrSyncUnsupported. fn runs concurrently and may hold a
// bucket or key lock, so it must not call into the store.
func (s *Store) OnSyncError(fn func(dir string, err error)) { s.onSyncError.Store(&fn) }

// flushLeft flushes dir, the directory a delete left, or its nearest parent still present
// when a concurrent prune removed it: that prune may not flush (expiry does not) or may
// flush only later, after this delete is answered.
func (s *Store) flushLeft(dir string) { s.flushDir(s.present(dir)) }

// present returns dir, or its nearest parent that still exists.
func (s *Store) present(dir string) string {
	for ; dir != "."; dir = filepath.Dir(dir) {
		if _, err := s.r.Lstat(dir); err == nil {
			break
		}
	}
	return dir
}

// SetSyncDirForTest replaces how every store flushes a directory, for tests outside this
// package; it returns the function that restores it. It is not safe while a store is in
// use: set it before starting anything that flushes, and restore it after that stops.
func SetSyncDirForTest(fn func(dir string) error) (restore func()) {
	old := syncDir
	syncDir = func(_ *os.Root, dir string) error { return fn(dir) }
	return func() { syncDir = old }
}

// SyncErrors returns how many directory flushes have failed since startup, refusals aside.
func (s *Store) SyncErrors() int64 { return s.syncErrors.Load() }

// place renames src to dst (both absolute, under the root), creating dst's parent, and
// syncs every directory that gained an entry. It retries when a concurrent delete prunes
// the freshly created parent.
func (s *Store) place(src, dst string, perm os.FileMode) error {
	var err error
	for range 4 {
		var top string
		if top, err = s.mkdirParent(dst, perm); err != nil {
			return err
		}
		err = rename(s.r, s.rel(src), s.rel(dst))
		if err == nil {
			s.syncUp(filepath.Dir(dst), top)
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
				return ErrConflict
			}
			return err
		}
	}
	return err
}

// syncUp syncs d and each parent of it up to and including top.
func (s *Store) syncUp(d, top string) {
	for ; ; d = filepath.Dir(d) {
		s.flushDir(s.rel(d))
		if d == top || len(d) <= len(s.root) {
			return
		}
	}
}

// mkdirParent creates dst's parent, mapping "a path component is a file" to ErrConflict.
// It returns the deepest directory that existed before, which gained the first new entry.
// Two writes creating the same directories at once may each see the other's as existing;
// journaling filesystems commit those entries with the later directory sync anyway.
func (s *Store) mkdirParent(dst string, perm os.FileMode) (string, error) {
	top := filepath.Dir(dst)
	if fi, err := s.r.Lstat(s.rel(top)); err == nil && fi.IsDir() {
		return top, nil
	}
	for len(top) > len(s.root) {
		if _, err := s.r.Lstat(s.rel(top)); !errors.Is(err, os.ErrNotExist) {
			break
		}
		top = filepath.Dir(top)
	}
	err := s.r.MkdirAll(s.rel(filepath.Dir(dst)), perm)
	if err != nil && (errors.Is(err, syscall.ENOTDIR) || errors.Is(err, os.ErrExist)) {
		return "", ErrConflict
	}
	return top, err
}

// pruneEmptyParents removes now-empty directories from dir up to, not including, the
// bucket directory, stopping at an explicit directory marker. It returns the directory
// that lost the last entry, the one to flush.
func (s *Store) pruneEmptyParents(bucket, dir string) string {
	stop := s.bucketPath(bucket)
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if s.hasMarker(bucket, filepath.ToSlash(dir[len(stop)+1:])+"/") {
			return dir
		}
		if s.r.Remove(s.rel(dir)) != nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return dir
}
