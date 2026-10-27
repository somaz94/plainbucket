// Package storage keeps S3 objects as plain files: object <key> of bucket <b> is the
// file <root>/<b>/<key>. Everything else lives under <root>/.plainbucket.
package storage

import (
	"crypto/md5" //nolint:gosec // S3 ETag format, not a security boundary
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// SysDir holds everything that is not an object. Bucket names cannot start with '.'.
	SysDir = ".plainbucket"

	maxKeyLen     = 1024
	maxSegmentLen = 255

	dirPerm    = 0o755
	filePerm   = 0o644
	sysDirPerm = 0o750

	// emptyETag is the MD5 of zero bytes, used for directory markers.
	emptyETag = `"d41d8cd98f00b204e9800998ecf8427e"`
)

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Errors returned by the store; the API layer maps them to S3 error codes.
var (
	ErrNoSuchBucket       = errors.New("no such bucket")
	ErrNoSuchKey          = errors.New("no such key")
	ErrBucketExists       = errors.New("bucket already exists")
	ErrBucketNotEmpty     = errors.New("bucket not empty")
	ErrInvalidBucketName  = errors.New("invalid bucket name")
	ErrInvalidKey         = errors.New("key cannot be stored as a path")
	ErrKeyTooLong         = errors.New("key too long")
	ErrConflict           = errors.New("key conflicts with an existing file or directory")
	ErrShortBody          = errors.New("body shorter than declared size")
	ErrPreconditionFailed = errors.New("precondition failed")
	ErrNoSuchUpload       = errors.New("no such upload")
	ErrInvalidPart        = errors.New("invalid part")
	ErrBadDigest          = errors.New("checksum does not match the object")
	ErrInvalidChecksum    = errors.New("checksum algorithm or type not valid for this upload")
	ErrInvalidPartOrder   = errors.New("parts not in ascending order")
	ErrEntityTooSmall     = errors.New("part too small")
	ErrMarkerNotEmpty     = errors.New("a key ending in / must have an empty body")
	ErrEntityTooLarge     = errors.New("object larger than the maximum size")
	ErrInvalidRange       = errors.New("range outside the source object")
	ErrLocked             = errors.New("the root is in use by another plainbucket process")
)

// ObjectInfo describes an object.
type ObjectInfo struct {
	Key         string
	Size        int64
	ModTime     time.Time
	ETag        string
	ContentType string
	UserMeta    map[string]string
	Headers     map[string]string // Cache-Control, Content-Disposition, ...
	Checksums   map[string]string // algorithm -> base64; "<digest>-<parts>" when ChecksumType is COMPOSITE
	// ChecksumType is COMPOSITE or FULL_OBJECT; empty means FULL_OBJECT.
	ChecksumType string
	Parts        []PartRecord // the parts a multipart upload was assembled from, in order
	Tags         []Tag
	IsDir        bool // a directory marker key ending in /
}

// Tag is one object or bucket tag.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PartRecord is one part of an object completed from a multipart upload.
type PartRecord struct {
	Size      int64             `json:"size"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

// Metadata is what a client sets on an object besides its data.
type Metadata struct {
	ContentType string            `json:"content_type,omitempty"`
	UserMeta    map[string]string `json:"user_meta,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Tags        []Tag             `json:"tags,omitempty"`
}

// sidecar is stored per object under SysDir/meta. It is trusted only while size and
// mtime still match the data file, so an edit made outside the server invalidates it.
type sidecar struct {
	Key          string            `json:"key"`
	ETag         string            `json:"etag"`
	Size         int64             `json:"size"`
	MtimeNs      int64             `json:"mtime_ns"`
	Checksums    map[string]string `json:"checksums,omitempty"`
	ChecksumType string            `json:"checksum_type,omitempty"`
	Parts        []PartRecord      `json:"parts,omitempty"`
	// Dir marks an explicit directory marker (a key ending in /); size and mtime are unused.
	Dir bool `json:"dir,omitempty"`
	Metadata
}

// Conditions are the If-Match / If-None-Match headers of a write.
type Conditions struct {
	IfMatch     string // ETag the current object must have
	IfNoneMatch string // "*": the key must not exist; an ETag: the current object must not have it
}

// Store is a filesystem-backed object store. Every path derived from a bucket name or
// key is resolved through an os.Root, so neither ".." nor a symlink planted in a bucket
// can reach outside the root directory.
type Store struct {
	root string
	r    *os.Root
	now  func() time.Time
	// MinPartSize is the smallest size allowed for every part but the last.
	MinPartSize int64

	locks       [64]sync.Mutex // per key
	uploadLocks [64]sync.Mutex // per multipart upload
	bucketMu    sync.Mutex
	recordMu    sync.Mutex               // serializes changes to bucket records
	buckets     map[string]*sync.RWMutex // writers hold RLock, bucket deletion holds Lock
	hashes      flight

	limits      atomic.Pointer[map[string]Limit] // swapped whole by SetLimits while serving
	usageMu     sync.Mutex
	bucketUsage map[string]Usage // tracked between janitor measurements
	measuring   map[string]Usage // changes made while a measurement runs
	resets      map[string]bool  // buckets created (true) or deleted (false) while it runs
	measureMu   sync.Mutex       // one measurement at a time, so they cannot swap each other's changes
	kick        chan struct{}    // wakes the janitor when an evicting bucket is over its limit
	expired     atomic.Int64
	evicted     atomic.Int64
	onLimit     atomic.Pointer[func(bucket, key string)] // see OnLimitRemove
	paused      atomic.Bool                              // see SetPaused
	lastScrub   atomic.Pointer[ScrubResult]

	probeMu   sync.Mutex
	probing   *probe                // the read a Probe in progress waits for
	probeRead func() (int64, error) // readRoot, replaced in tests

	trashAfter atomic.Pointer[map[string]time.Duration] // swapped whole by SetTrash
	trashMu    sync.Mutex                               // serializes restores, purges and the sweep's expiry

	minFree atomic.Int64 // bytes a write must leave free
	pending atomic.Int64 // bytes reserved by writes in progress
	statfs  func(path string) (free, size int64, err error)

	lockFile *os.File // held by Lock

	usage atomic.Pointer[Usage]
}

// New opens the store rooted at root, creating its system directories.
func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s := storeAt(abs)
	for _, d := range []string{"meta", "tmp", "uploads", "buckets", "trash"} {
		if err := os.MkdirAll(s.sys(d), sysDirPerm); err != nil {
			return nil, err
		}
	}
	if s.r, err = os.OpenRoot(abs); err != nil {
		return nil, err
	}
	s.loadScrub()
	return s, nil
}

// OpenReadOnly opens an existing root for reading without creating anything in it, so it
// works on a copy or a read-only volume. Only reads may be called on the result.
func OpenReadOnly(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s := storeAt(abs)
	if s.r, err = os.OpenRoot(abs); err != nil {
		return nil, err
	}
	return s, nil
}

func storeAt(abs string) *Store {
	s := &Store{root: abs, now: time.Now, MinPartSize: 5 << 20, buckets: map[string]*sync.RWMutex{},
		bucketUsage: map[string]Usage{}, kick: make(chan struct{}, 1), statfs: diskSpace}
	s.probeRead = s.readRoot
	return s
}

// Close releases the root directory handle.
func (s *Store) Close() error {
	if s.lockFile != nil {
		_ = s.lockFile.Close()
	}
	return s.r.Close()
}

// Root returns the absolute root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) sys(parts ...string) string {
	return filepath.Join(append([]string{s.root, SysDir}, parts...)...)
}

// rel converts an absolute path under the root to the form os.Root expects.
func (s *Store) rel(p string) string {
	r, err := filepath.Rel(s.root, p)
	if err != nil {
		return p
	}
	return r
}

// CaseInsensitive reports whether the filesystem under root folds case, which makes
// keys differing only in case collide.
func (s *Store) CaseInsensitive() bool {
	f, err := os.CreateTemp(s.sys("tmp"), "probe-Case-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()
	dir, base := filepath.Split(name)
	_, err = os.Stat(filepath.Join(dir, strings.ToLower(base)))
	return err == nil
}

// ValidKey reports whether key can name an object.
func ValidKey(key string) bool { return checkKey(key) == nil }

// ValidBucketName reports whether name follows the S3 bucket naming rules.
func ValidBucketName(name string) bool {
	return bucketNameRe.MatchString(name) && !strings.Contains(name, "..")
}

// checkKey validates that key maps to a path inside its bucket.
func checkKey(key string) error {
	if len(key) > maxKeyLen {
		return ErrKeyTooLong
	}
	k := strings.TrimSuffix(key, "/")
	if k == "" || strings.ContainsRune(key, 0) {
		return ErrInvalidKey
	}
	for seg := range strings.SplitSeq(k, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return ErrInvalidKey
		case len(seg) > maxSegmentLen:
			return ErrKeyTooLong
		}
	}
	return nil
}

func (s *Store) bucketPath(bucket string) string { return filepath.Join(s.root, bucket) }

func (s *Store) dataPath(bucket, key string) string {
	return filepath.Join(s.root, bucket, filepath.FromSlash(strings.TrimSuffix(key, "/")))
}

// metaPath hashes the key so sidecars never collide with each other the way a mirrored
// tree would (the sidecar of "a" against the directory of "a.json/b").
func (s *Store) metaPath(bucket, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return s.sys("meta", bucket, h[:2], h+".json")
}

func stripe(locks *[64]sync.Mutex, id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	m := &locks[h.Sum32()%uint32(len(locks))]
	m.Lock()
	return m.Unlock
}

func (s *Store) lock(bucket, key string) func() { return stripe(&s.locks, bucket+"/"+key) }

func (s *Store) lockUpload(id string) func() { return stripe(&s.uploadLocks, id) }

func (s *Store) bucketLock(bucket string) *sync.RWMutex {
	s.bucketMu.Lock()
	defer s.bucketMu.Unlock()
	l, ok := s.buckets[bucket]
	if !ok {
		l = &sync.RWMutex{}
		s.buckets[bucket] = l
	}
	return l
}

// requireBucket returns ErrNoSuchBucket unless bucket is an existing directory.
func (s *Store) requireBucket(bucket string) error {
	if !ValidBucketName(bucket) {
		return ErrNoSuchBucket
	}
	fi, err := s.r.Stat(bucket)
	if err != nil || !fi.IsDir() {
		return ErrNoSuchBucket
	}
	return nil
}

func (s *Store) readSidecar(bucket, key string) (sidecar, bool) {
	var m sidecar
	raw, err := os.ReadFile(s.metaPath(bucket, key))
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Key != key {
		return sidecar{}, false
	}
	return m, true
}

// hasMarker reports whether dirKey ("a/b/") was created as an explicit directory marker.
func (s *Store) hasMarker(bucket, dirKey string) bool {
	m, ok := s.readSidecar(bucket, dirKey)
	return ok && m.Dir
}

func (m sidecar) matches(fi os.FileInfo) bool {
	return m.ETag != "" && m.Size == fi.Size() && m.MtimeNs == fi.ModTime().UnixNano()
}

// syntheticETag stands in for an MD5 the server has not computed. The "-1" suffix marks
// it as a non-MD5 ETag, so clients do not compare it with a local MD5.
func syntheticETag(fi os.FileInfo) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%d", fi.Size(), fi.ModTime().UnixNano()))
	return `"` + hex.EncodeToString(sum[:16]) + `-1"`
}

func md5ETag(sum []byte) string { return `"` + hex.EncodeToString(sum) + `"` }

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

func (m sidecar) info(mod time.Time) ObjectInfo {
	return ObjectInfo{
		Key: m.Key, Size: m.Size, ModTime: mod, ETag: m.ETag,
		ContentType: m.ContentType, UserMeta: m.UserMeta, Headers: m.Headers, Checksums: m.Checksums,
		ChecksumType: m.ChecksumType, Parts: m.Parts, Tags: m.Tags,
	}
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

func trimETag(e string) string { return strings.Trim(strings.TrimPrefix(e, "W/"), `"`) }

func (s *Store) stageSidecar(m sidecar) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return s.stageBytes("meta-*", raw)
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

// escapesRoot reports an os.Root refusal: the path left the root through ".." or a symlink.
// os.Root returns an unexported error for it, so only its message identifies it.
func escapesRoot(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}
