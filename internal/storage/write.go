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

// receive streams exactly size bytes from body into a temporary file and returns its
// path, MD5 and verified checksums. The caller removes the file.
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

	fi, err := os.Stat(tmp)
	if err != nil {
		return ObjectInfo{}, err
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
	m.Key, m.Size, m.MtimeNs = key, fi.Size(), fi.ModTime().UnixNano()
	metaTmp, err := s.stageSidecar(m)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("writing metadata: %w", err)
	}
	defer func() { _ = os.Remove(metaTmp) }()

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

// checkConditions runs with the key lock held, so it must not persist a refreshed sidecar.
func (s *Store) checkConditions(bucket, key string, cond Conditions) error {
	if cond.IfMatch == "" && cond.IfNoneMatch == "" {
		return nil
	}
	f, info, err := s.open(bucket, key, false)
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

// rename is os.Root.Rename; tests replace it to fail a specific placement.
var rename = func(r *os.Root, src, dst string) error { return r.Rename(src, dst) }

// place renames src to dst (both absolute, under the root), creating dst's parent. It
// retries when a concurrent delete prunes the freshly created parent.
func (s *Store) place(src, dst string, perm os.FileMode) error {
	var err error
	for range 4 {
		if err = s.mkdirParent(dst, perm); err != nil {
			return err
		}
		if err = rename(s.r, s.rel(src), s.rel(dst)); !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
				return ErrConflict
			}
			return err
		}
	}
	return err
}

// mkdirParent creates dst's parent, mapping "a path component is a file" to ErrConflict.
func (s *Store) mkdirParent(dst string, perm os.FileMode) error {
	err := s.r.MkdirAll(s.rel(filepath.Dir(dst)), perm)
	if err != nil && (errors.Is(err, syscall.ENOTDIR) || errors.Is(err, os.ErrExist)) {
		return ErrConflict
	}
	return err
}

// pruneEmptyParents removes now-empty directories from dir up to, not including, the
// bucket directory, stopping at an explicit directory marker.
func (s *Store) pruneEmptyParents(bucket, dir string) {
	stop := s.bucketPath(bucket)
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if s.hasMarker(bucket, filepath.ToSlash(dir[len(stop)+1:])+"/") {
			return
		}
		if s.r.Remove(s.rel(dir)) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
