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
		return s.putMarker(bucket, key, o.Cond, o.Metadata.Tags)
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
func (s *Store) putMarker(bucket, key string, cond Conditions, tags []Tag) (ObjectInfo, error) {
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
	dir := s.dataPath(bucket, key)
	p := s.rel(dir)
	top, err := s.mkdirParent(filepath.Join(dir, "x"), dirPerm)
	if err != nil {
		return ObjectInfo{}, err
	}
	s.syncUp(dir, top)
	raw, err := json.Marshal(sidecar{Key: key, ETag: emptyETag, Dir: true, Metadata: Metadata{Tags: tags}})
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
	info := markerInfo(key, mod)
	info.Tags = tags
	return info, nil
}

// emptyDir reports whether the directory at p, relative to the root, holds nothing.
func (s *Store) emptyDir(p string) bool {
	d, err := s.r.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = d.Close() }()
	_, err = d.Readdirnames(1)
	return errors.Is(err, io.EOF)
}

func markerInfo(key string, mod time.Time) ObjectInfo {
	return ObjectInfo{Key: key, ModTime: mod, ETag: emptyETag, ContentType: DirContentType, IsDir: true}
}

// GetObject opens bucket/key for reading and records the read as its last use. The
// file is nil for a directory marker.
func (s *Store) GetObject(bucket, key string) (*os.File, ObjectInfo, error) {
	f, info, err := s.open(bucket, key, true, false)
	if f != nil {
		s.touch(bucket, key)
	}
	return f, info, err
}

// ReadObject opens bucket/key like GetObject without counting as a use, for the server's
// own copies, which must not keep an object from expiring.
func (s *Store) ReadObject(bucket, key string) (*os.File, ObjectInfo, error) {
	// Saving a rebuilt sidecar would move its mtime, which is the object's last use.
	return s.open(bucket, key, false, false)
}

// open opens bucket/key. persist lets it save a rebuilt sidecar. locked says the caller
// holds the key lock, so open skips the recheck; such a caller must pass persist false.
func (s *Store) open(bucket, key string, persist, locked bool) (*os.File, ObjectInfo, error) {
	if err := s.requireBucket(bucket); err != nil {
		return nil, ObjectInfo{}, err
	}
	if checkKey(key) != nil {
		return nil, ObjectInfo{}, ErrNoSuchKey
	}
	p := s.rel(s.dataPath(bucket, key))
	if isDirKey(key) {
		// Lstat: the listing skips a symlink to a directory, so a read must too.
		fi, err := s.r.Lstat(p)
		if err != nil || !fi.IsDir() {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		m, ok := s.readSidecar(bucket, key)
		// As listed: a directory made without a marker is one only while it is empty.
		if !(ok && m.Dir) && !s.emptyDir(p) {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		info := markerInfo(key, fi.ModTime())
		if ok {
			info.Tags = m.Tags
		}
		return nil, info, nil
	}

	f, fi, err := s.openData(bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	m, ok := s.readSidecar(bucket, key)
	if (!ok || !m.matches(fi)) && !locked {
		// Between a commit's two renames the new data has no matching sidecar yet; under the
		// key lock the commit has finished, so look again before taking the file for edited.
		_ = f.Close()
		unlock := s.lock(bucket, key)
		f, fi, err = s.openData(bucket, key)
		if err == nil {
			m, ok = s.readSidecar(bucket, key)
		}
		unlock()
		if err != nil {
			return nil, ObjectInfo{}, err
		}
	}
	if !ok || !m.matches(fi) {
		if m, err = s.refresh(bucket, key, f, fi, m, persist); err != nil {
			_ = f.Close()
			return nil, ObjectInfo{}, err
		}
	}
	return f, m.info(fi.ModTime()), nil
}

// openData opens the data file of bucket/key, a key not ending in /, whose bucket and
// key the caller has checked.
func (s *Store) openData(bucket, key string) (*os.File, os.FileInfo, error) {
	f, err := s.r.Open(s.rel(s.dataPath(bucket, key)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || escapesRoot(err) {
			return nil, nil, ErrNoSuchKey
		}
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = ErrNoSuchKey
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
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
	_, err := s.DeleteObjectIf(bucket, key, DeleteConditions{})
	return err
}

// DeleteConditions make a delete conditional. A missing object is not an error, whatever
// they say, as on S3.
type DeleteConditions struct {
	// IfMatch is the ETag the object must have, or "*".
	IfMatch string
	// Size is the size the object must have, checked when HasSize is set.
	Size    int64
	HasSize bool
	// LastModified is the modification time the object must have, to the second; the
	// zero time skips the check.
	LastModified time.Time
}

func (c DeleteConditions) any() bool { return c.IfMatch != "" || c.HasSize || !c.LastModified.IsZero() }

func (c DeleteConditions) check(info ObjectInfo) error {
	switch {
	case c.IfMatch != "" && c.IfMatch != "*" && trimETag(c.IfMatch) != trimETag(info.ETag),
		c.HasSize && c.Size != info.Size,
		!c.LastModified.IsZero() && !c.LastModified.Truncate(time.Second).Equal(info.ModTime.Truncate(time.Second)):
		return ErrPreconditionFailed
	}
	return nil
}

// DeleteObjectIf deletes bucket/key when it meets cond; removed reports whether there
// was an object to delete. The directory it removed an entry from is flushed before it
// returns, so an acknowledged delete survives a power loss.
func (s *Store) DeleteObjectIf(bucket, key string, cond DeleteConditions) (removed bool, err error) {
	return s.deleteObject(bucket, key, cond, s.flushLeft)
}

// Deletes runs deletes whose directory flushes wait for Flush, which syncs each
// directory they changed once: a DeleteObjects of many keys in one directory needs one.
// It is for one goroutine.
type Deletes struct {
	s    *Store
	dirs map[string]bool
}

// Deletes starts a batch of deletes.
func (s *Store) Deletes() *Deletes { return &Deletes{s: s, dirs: map[string]bool{}} }

// DeleteObjectIf is Store.DeleteObjectIf with the flush left to Flush.
func (d *Deletes) DeleteObjectIf(bucket, key string, cond DeleteConditions) (removed bool, err error) {
	return d.s.deleteObject(bucket, key, cond, func(dir string) { d.dirs[dir] = true })
}

// Flush syncs the directories the deletes so far changed, each once.
func (d *Deletes) Flush() {
	done := map[string]bool{}
	for dir := range d.dirs {
		if left := d.s.present(dir); !done[left] {
			done[left] = true
			d.s.flushDir(left)
		}
	}
	clear(d.dirs)
}

// deleteObject is DeleteObjectIf with the directory flush handed to flush.
func (s *Store) deleteObject(bucket, key string, cond DeleteConditions, flush func(dir string)) (removed bool, err error) {
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	if err := s.requireBucket(bucket); err != nil {
		return false, err
	}
	if checkKey(key) != nil {
		return false, nil
	}
	unlock := s.lock(bucket, key)
	defer unlock()
	if cond.any() {
		f, info, err := s.open(bucket, key, false, true)
		if f != nil {
			_ = f.Close()
		}
		if errors.Is(err, ErrNoSuchKey) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := cond.check(info); err != nil {
			return false, err
		}
	}

	p := s.dataPath(bucket, key)
	if isDirKey(key) {
		hadMarker := os.Remove(s.metaPath(bucket, key)) == nil
		// The path without the slash may be a file: the object key, not this one.
		if fi, err := s.r.Lstat(s.rel(p)); err != nil || !fi.IsDir() {
			return false, nil
		}
		if s.r.Remove(s.rel(p)) != nil {
			return hadMarker, nil // a folder with objects in it loses only its marker
		}
		flush(s.rel(s.pruneEmptyParents(bucket, filepath.Dir(p))))
		return true, nil
	}
	fi, err := s.r.Lstat(s.rel(p))
	if err != nil || fi.IsDir() {
		_ = os.Remove(s.metaPath(bucket, key))
		return false, nil
	}
	if after := s.TrashAfter(bucket); after > 0 && fi.Mode().IsRegular() {
		return s.trashFile(bucket, key, p, fi, after, flush)
	}
	return s.removeFile(bucket, key, p, fi, flush)
}

// removeFile deletes the object file p, described by fi, with its sidecar and the
// directories it leaves empty, then passes the directory left to flush unless it is nil;
// removed is false when the file was already gone. The caller holds the key lock.
func (s *Store) removeFile(bucket, key, p string, fi os.FileInfo, flush func(dir string)) (removed bool, err error) {
	if err := s.r.Remove(s.rel(p)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if fi.Mode().IsRegular() {
		s.adjust(bucket, -1, -fi.Size())
	}
	_ = os.Remove(s.metaPath(bucket, key))
	left := s.pruneEmptyParents(bucket, filepath.Dir(p))
	if flush != nil {
		flush(s.rel(left))
	}
	return true, nil
}

// CopyOptions configure CopyObject.
type CopyOptions struct {
	// ReplaceMetadata uses Metadata instead of the source object's.
	ReplaceMetadata bool
	Metadata        Metadata
	// ReplaceTags uses Tags instead of the source object's.
	ReplaceTags    bool
	Tags           []Tag
	Cond           Conditions // on the destination
	SrcIfMatch     string
	SrcIfNoneMatch string
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
	md.Tags = src.Tags
	if o.ReplaceTags {
		md.Tags = o.Tags
	}
	if src.IsDir {
		return s.PutObject(dstBucket, dstKey, strings.NewReader(""), 0, PutOptions{Cond: o.Cond, Metadata: Metadata{Tags: md.Tags}})
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
