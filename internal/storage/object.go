package storage

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 ETag format
	"encoding/json"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DirContentType is reported for directory markers, as the S3 console does.
const DirContentType = "application/x-directory"

// PutOptions configure PutObject.
type PutOptions struct {
	Metadata
	Cond   Conditions
	Verify Verifier
}

func isDirKey(key string) bool { return strings.HasSuffix(key, "/") }

// PutObject stores exactly size bytes from body as bucket/key, replacing it atomically;
// a negative size reads body to its end. A key ending in / with an empty body creates a
// directory.
func (s *Store) PutObject(bucket, key string, body io.Reader, size int64, o PutOptions) (ObjectInfo, error) {
	if err := s.requireBucket(bucket); err != nil {
		return ObjectInfo{}, err
	}
	if err := checkKey(key); err != nil {
		return ObjectInfo{}, err
	}
	if isDirKey(key) && size > 0 {
		return ObjectInfo{}, ErrMarkerNotEmpty
	}
	if err := s.fits(bucket, key, size); err != nil {
		return ObjectInfo{}, err
	}
	tmp, sum, checks, err := s.receive(body, size, o.Verify)
	if err != nil {
		return ObjectInfo{}, err
	}
	if size < 0 {
		if err := s.fitsReceived(bucket, key, tmp); err != nil {
			_ = os.Remove(tmp)
			return ObjectInfo{}, err
		}
	}
	if isDirKey(key) {
		_ = os.Remove(tmp)
		return s.putMarker(bucket, key, o.Cond)
	}
	return s.commit(bucket, key, tmp, sidecar{ETag: md5ETag(sum), Checksums: checks, Metadata: o.Metadata}, o.Cond)
}

// fitsReceived checks the limits against a body whose size was unknown until received.
func (s *Store) fitsReceived(bucket, key, tmp string) error {
	fi, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if isDirKey(key) && fi.Size() != 0 {
		return ErrMarkerNotEmpty
	}
	// The body is already on disk, so only what is left counts.
	release, _, err := s.reserve(0)
	if err != nil {
		return err
	}
	release()
	return s.fits(bucket, key, fi.Size())
}

// putMarker creates the directory for a key ending in / and records it as an explicit
// marker, so deleting the last object below it does not remove it.
func (s *Store) putMarker(bucket, key string, cond Conditions) (ObjectInfo, error) {
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
	p := s.rel(s.dataPath(bucket, key))
	if err := s.r.MkdirAll(p, dirPerm); err != nil {
		if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, os.ErrExist) {
			return ObjectInfo{}, ErrConflict
		}
		return ObjectInfo{}, err
	}
	raw, err := json.Marshal(sidecar{Key: key, ETag: emptyETag, Dir: true})
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := s.writeFileAtomic(s.metaPath(bucket, key), raw); err != nil {
		return ObjectInfo{}, err
	}
	mod := s.now()
	if fi, err := s.r.Stat(p); err == nil {
		mod = fi.ModTime()
	}
	return markerInfo(key, mod), nil
}

func markerInfo(key string, mod time.Time) ObjectInfo {
	return ObjectInfo{Key: key, ModTime: mod, ETag: emptyETag, ContentType: DirContentType, IsDir: true}
}

// GetObject opens bucket/key for reading and records the read as its last use. The
// file is nil for a directory marker.
func (s *Store) GetObject(bucket, key string) (*os.File, ObjectInfo, error) {
	f, info, err := s.open(bucket, key, true)
	if f != nil {
		s.touch(bucket, key)
	}
	return f, info, err
}

// open opens bucket/key. persist lets it save a rebuilt sidecar, which takes the key
// lock, so callers already holding that lock pass false.
func (s *Store) open(bucket, key string, persist bool) (*os.File, ObjectInfo, error) {
	if err := s.requireBucket(bucket); err != nil {
		return nil, ObjectInfo{}, err
	}
	if checkKey(key) != nil {
		return nil, ObjectInfo{}, ErrNoSuchKey
	}
	p := s.rel(s.dataPath(bucket, key))
	if isDirKey(key) {
		fi, err := s.r.Stat(p)
		if err != nil || !fi.IsDir() {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		return nil, markerInfo(key, fi.ModTime()), nil
	}

	f, err := s.r.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || escapesRoot(err) {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		return nil, ObjectInfo{}, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = ErrNoSuchKey
	}
	if err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, err
	}
	m, ok := s.readSidecar(bucket, key)
	if !ok || !m.matches(fi) {
		if m, err = s.refresh(bucket, key, f, fi, m, persist); err != nil {
			_ = f.Close()
			return nil, ObjectInfo{}, err
		}
	}
	return f, m.info(fi.ModTime()), nil
}

// HeadObject returns the object's metadata.
func (s *Store) HeadObject(bucket, key string) (ObjectInfo, error) {
	f, info, err := s.GetObject(bucket, key)
	if f != nil {
		_ = f.Close()
	}
	return info, err
}

// hashFile is replaced in tests to count how often a file is hashed.
var hashFile = func(r io.Reader) ([]byte, error) {
	h := md5.New() //nolint:gosec // S3 ETag format
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// refresh rebuilds the sidecar of a file written outside the server: it computes the MD5
// and keeps the old metadata, or guesses the content type from the extension.
func (s *Store) refresh(bucket, key string, f *os.File, fi os.FileInfo, old sidecar, persist bool) (sidecar, error) {
	// Concurrent readers of the same unchanged file share one hash: a HEAD storm on a
	// large file copied in from outside would otherwise read it once per request.
	id := bucket + "/" + key + "\x00" + strconv.FormatInt(fi.Size(), 10) + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	sum, err := s.hashes.do(id, func() ([]byte, error) { return hashFile(f) })
	if err != nil {
		return sidecar{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return sidecar{}, err
	}
	m := sidecar{Key: key, ETag: md5ETag(sum), Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), Metadata: old.Metadata}
	if m.ContentType == "" {
		m.ContentType = mime.TypeByExtension(filepath.Ext(key))
	}
	if !persist {
		return m, nil
	}
	now, err := f.Stat()
	if err != nil || !m.matches(now) {
		return m, nil // changed while hashing
	}
	// Persist only if the hashed file is still the one at the path and no upload landed a
	// sidecar for it meanwhile; otherwise a GET racing a PUT would erase the PUT's metadata.
	unlock := s.lock(bucket, key)
	defer unlock()
	cur, err := s.r.Stat(s.rel(s.dataPath(bucket, key)))
	if err != nil || !os.SameFile(cur, now) {
		return m, nil
	}
	if ex, ok := s.readSidecar(bucket, key); ok && ex.matches(cur) {
		return ex, nil
	}
	if raw, err := json.Marshal(m); err == nil {
		_ = s.writeFileAtomic(s.metaPath(bucket, key), raw)
	}
	return m, nil
}

// DeleteObject removes bucket/key; a missing key is not an error, as in S3. Directories
// left empty are removed up to the bucket or the nearest explicit directory marker, so
// an implicit prefix disappears with its last object.
func (s *Store) DeleteObject(bucket, key string) error {
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	if err := s.requireBucket(bucket); err != nil {
		return err
	}
	if checkKey(key) != nil {
		return nil
	}
	unlock := s.lock(bucket, key)
	defer unlock()

	p := s.dataPath(bucket, key)
	if isDirKey(key) {
		_ = os.Remove(s.metaPath(bucket, key))
		if s.r.Remove(s.rel(p)) == nil {
			s.pruneEmptyParents(bucket, filepath.Dir(p))
		}
		return nil
	}
	fi, err := s.r.Lstat(s.rel(p))
	if err != nil || fi.IsDir() {
		_ = os.Remove(s.metaPath(bucket, key))
		return nil
	}
	return s.removeFile(bucket, key, p, fi)
}

// removeFile deletes the object file p, described by fi, with its sidecar and the
// directories it leaves empty. The caller holds the key lock.
func (s *Store) removeFile(bucket, key, p string, fi os.FileInfo) error {
	if err := s.r.Remove(s.rel(p)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Mode().IsRegular() {
		s.adjust(bucket, -1, -fi.Size())
	}
	_ = os.Remove(s.metaPath(bucket, key))
	s.pruneEmptyParents(bucket, filepath.Dir(p))
	return nil
}

// CopyOptions configure CopyObject.
type CopyOptions struct {
	// ReplaceMetadata uses Metadata instead of the source object's.
	ReplaceMetadata bool
	Metadata        Metadata
	Cond            Conditions // on the destination
	SrcIfMatch      string
	SrcIfNoneMatch  string
}

func (o CopyOptions) checkSource(src ObjectInfo) error {
	if o.SrcIfMatch != "" && trimETag(o.SrcIfMatch) != trimETag(src.ETag) {
		return ErrPreconditionFailed
	}
	if o.SrcIfNoneMatch != "" && (o.SrcIfNoneMatch == "*" || trimETag(o.SrcIfNoneMatch) == trimETag(src.ETag)) {
		return ErrPreconditionFailed
	}
	return nil
}

// CopyObject copies srcBucket/srcKey to dstBucket/dstKey; the copy stops when ctx is done.
func (s *Store) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, o CopyOptions) (ObjectInfo, error) {
	f, src, err := s.GetObject(srcBucket, srcKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	if err := o.checkSource(src); err != nil {
		return ObjectInfo{}, err
	}
	md := Metadata{ContentType: src.ContentType, UserMeta: src.UserMeta, Headers: src.Headers}
	if o.ReplaceMetadata {
		md = o.Metadata
	}
	if src.IsDir {
		return s.PutObject(dstBucket, dstKey, strings.NewReader(""), 0, PutOptions{Cond: o.Cond})
	}
	if err := s.requireBucket(dstBucket); err != nil {
		return ObjectInfo{}, err
	}
	if err := checkKey(dstKey); err != nil {
		return ObjectInfo{}, err
	}
	if isDirKey(dstKey) {
		return ObjectInfo{}, ErrMarkerNotEmpty
	}
	if err := s.fits(dstBucket, dstKey, src.Size); err != nil {
		return ObjectInfo{}, err
	}
	tmp, sum, _, err := s.receive(ctxReader{ctx, f}, src.Size, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	return s.commit(dstBucket, dstKey, tmp, sidecar{ETag: md5ETag(sum), Checksums: copiedChecksums(src), Metadata: md}, o.Cond)
}

// copiedChecksums keeps the source's full-object checksums: a copy is a single part, so a
// composite checksum of the source's parts does not describe it.
func copiedChecksums(src ObjectInfo) map[string]string {
	if src.ChecksumType == ChecksumComposite {
		return nil
	}
	return src.Checksums
}
