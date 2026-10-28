package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// ListBucketsWithin is ListBuckets failing with ErrProbeTimeout after timeout, for callers
// that must answer while a network mount hangs. Calls made meanwhile share one listing.
func (s *Store) ListBucketsWithin(timeout time.Duration) ([]BucketInfo, error) {
	b, err := s.listings.wait(timeout, s.listRead)
	return slices.Clone(b), err
}

// bucketRecord is what the server keeps about a bucket under SysDir/buckets.
type bucketRecord struct {
	Created time.Time `json:"created"`
	Tags    []Tag     `json:"tags,omitempty"`
}

func (s *Store) readBucketRecord(name string) (bucketRecord, bool) {
	var rec bucketRecord
	raw, err := os.ReadFile(s.sys("buckets", name+".json"))
	return rec, err == nil && json.Unmarshal(raw, &rec) == nil
}

// bucketCreated reads the recorded creation time. A directory created outside the
// server falls back to its mtime, which SetBucketTags then freezes into the record.
func (s *Store) bucketCreated(name string, fi os.FileInfo) time.Time {
	if rec, ok := s.readBucketRecord(name); ok {
		return rec.Created
	}
	return fi.ModTime()
}

// BucketTags returns the tags of bucket, nil when it has none.
func (s *Store) BucketTags(bucket string) ([]Tag, error) {
	if err := s.requireBucket(bucket); err != nil {
		return nil, err
	}
	rec, _ := s.readBucketRecord(bucket)
	return rec.Tags, nil
}

// SetBucketTags replaces the tags of bucket; nil removes them.
func (s *Store) SetBucketTags(bucket string, tags []Tag) error {
	// Shared, not exclusive: a pending Lock behind a long RLock holder (SetTags hashing an
	// edited object) would block every commit in the bucket. recordMu serializes updates.
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	if err := s.requireBucket(bucket); err != nil {
		return err
	}
	rec, ok := s.readBucketRecord(bucket)
	if !ok {
		if len(tags) == 0 {
			return nil // nothing to store; writing would freeze the creation time
		}
		fi, err := s.r.Stat(bucket)
		if err != nil {
			return err
		}
		rec.Created = fi.ModTime().UTC()
	}
	rec.Tags = tags
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.writeFileAtomic(s.sys("buckets", bucket+".json"), raw)
}

// CreateBucket creates the bucket directory.
func (s *Store) CreateBucket(name string) error { return s.CreateBucketWithTags(name, nil) }

// CreateBucketWithTags creates bucket name with its tags recorded at once.
func (s *Store) CreateBucketWithTags(name string, tags []Tag) error {
	if !ValidBucketName(name) {
		return ErrInvalidBucketName
	}
	bl := s.bucketLock(name)
	bl.Lock()
	defer bl.Unlock()
	if err := s.r.Mkdir(name, dirPerm); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrBucketExists
		}
		return err
	}
	s.flushDir(".")
	raw, _ := json.Marshal(bucketRecord{Created: s.now().UTC(), Tags: tags})
	_ = s.writeFileAtomic(s.sys("buckets", name+".json"), raw)
	s.usageMu.Lock()
	s.resetUsage(name, true) // listed in the usage before its first write
	s.usageMu.Unlock()
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
	s.usageMu.Lock()
	s.resetUsage(name, false)
	s.usageMu.Unlock()
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
