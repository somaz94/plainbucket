// Package storage keeps S3 objects as plain files: object <key> of bucket <b> is the
// file <root>/<b>/<key>. Everything else lives under <root>/.plainbucket.
package storage

import (
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	onLimit     atomic.Pointer[func(bucket, key string)]    // see OnLimitRemove
	onSyncError atomic.Pointer[func(dir string, err error)] // see OnSyncError
	syncErrors  atomic.Int64
	paused      atomic.Bool // see SetPaused
	lastScrub   atomic.Pointer[ScrubResult]

	probes    shared[int64]
	probeRead func() (int64, error) // readRoot, replaced in tests
	spaces    shared[spaceReading]

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
