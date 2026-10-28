package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

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
	Key     string `json:"key"`
	ETag    string `json:"etag"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	// Ino is the data file's inode: a mismatch sends an unlocked read to the locked
	// recheck, where size and mtime decide. 0 when unknown (older sidecars, other platforms).
	Ino          uint64            `json:"ino,omitempty"`
	Checksums    map[string]string `json:"checksums,omitempty"`
	ChecksumType string            `json:"checksum_type,omitempty"`
	Parts        []PartRecord      `json:"parts,omitempty"`
	// Dir marks an explicit directory marker (a key ending in /); size and mtime are unused.
	Dir bool `json:"dir,omitempty"`
	Metadata
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

// sameInode reports whether fi is the file m was recorded for, as far as inodes tell.
func (m sidecar) sameInode(fi os.FileInfo) bool {
	ino := inode(fi)
	return m.Ino == 0 || ino == 0 || m.Ino == ino
}

// syntheticETag stands in for an MD5 the server has not computed. The "-1" suffix marks
// it as a non-MD5 ETag, so clients do not compare it with a local MD5.
func syntheticETag(fi os.FileInfo) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%d", fi.Size(), fi.ModTime().UnixNano()))
	return `"` + hex.EncodeToString(sum[:16]) + `-1"`
}

func md5ETag(sum []byte) string { return `"` + hex.EncodeToString(sum) + `"` }

func (m sidecar) info(mod time.Time) ObjectInfo {
	return ObjectInfo{
		Key: m.Key, Size: m.Size, ModTime: mod, ETag: m.ETag,
		ContentType: m.ContentType, UserMeta: m.UserMeta, Headers: m.Headers, Checksums: m.Checksums,
		ChecksumType: m.ChecksumType, Parts: m.Parts, Tags: m.Tags,
	}
}

func trimETag(e string) string { return strings.Trim(strings.TrimPrefix(e, "W/"), `"`) }

func (s *Store) stageSidecar(m sidecar) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return s.stageBytes("meta-*", raw)
}
