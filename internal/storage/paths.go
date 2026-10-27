package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

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

// escapesRoot reports an os.Root refusal: the path left the root through ".." or a symlink.
// os.Root returns an unexported error for it, so only its message identifies it.
func escapesRoot(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}
