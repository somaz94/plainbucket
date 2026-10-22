package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name    string
	Created time.Time
}

// ListBuckets returns every directory under the root whose name is a valid bucket name,
// so directories that existed before the server started are buckets too.
func (s *Store) ListBuckets() ([]BucketInfo, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []BucketInfo
	for _, e := range ents {
		if !ValidBucketName(e.Name()) {
			continue
		}
		fi, err := s.r.Stat(e.Name())
		if err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, BucketInfo{Name: e.Name(), Created: s.bucketCreated(e.Name(), fi)})
	}
	return out, nil
}

// bucketCreated reads the creation time recorded by CreateBucket. A directory created
// outside the server falls back to its mtime.
func (s *Store) bucketCreated(name string, fi os.FileInfo) time.Time {
	var rec struct {
		Created time.Time `json:"created"`
	}
	if raw, err := os.ReadFile(s.sys("buckets", name+".json")); err == nil && json.Unmarshal(raw, &rec) == nil {
		return rec.Created
	}
	return fi.ModTime()
}

// CreateBucket creates the bucket directory.
func (s *Store) CreateBucket(name string) error {
	if !ValidBucketName(name) {
		return ErrInvalidBucketName
	}
	if err := s.r.Mkdir(name, dirPerm); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrBucketExists
		}
		return err
	}
	raw, _ := json.Marshal(map[string]time.Time{"created": s.now().UTC()})
	_ = s.writeFileAtomic(s.sys("buckets", name+".json"), raw)
	return nil
}

// HeadBucket returns ErrNoSuchBucket unless the bucket exists.
func (s *Store) HeadBucket(name string) error { return s.requireBucket(name) }

// DeleteBucket removes an empty bucket with its metadata and in-progress uploads.
func (s *Store) DeleteBucket(name string) error {
	bl := s.bucketLock(name)
	bl.Lock()
	defer bl.Unlock()
	if err := s.requireBucket(name); err != nil {
		return err
	}
	if err := s.r.Remove(name); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, os.ErrExist) {
			return ErrBucketNotEmpty
		}
		return err
	}
	_ = os.RemoveAll(s.sys("meta", name))
	_ = os.Remove(s.sys("buckets", name+".json"))
	ups, _ := os.ReadDir(s.sys("uploads"))
	for _, e := range ups {
		var u uploadMeta
		raw, err := os.ReadFile(filepath.Join(s.sys("uploads", e.Name()), uploadMetaFile))
		if err == nil && json.Unmarshal(raw, &u) == nil && u.Bucket == name {
			_ = os.RemoveAll(s.sys("uploads", e.Name()))
		}
	}
	return nil
}
