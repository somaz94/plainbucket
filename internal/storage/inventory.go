package storage

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"time"
)

// Values of InventoryEntry.Metadata: where its ETag, checksum and content type come from.
const (
	MetadataRecorded = "recorded" // the server's record, which still matches the file
	MetadataEdited   = "edited"   // the file alone: its record no longer matches, changed outside the server
	MetadataMissing  = "missing"  // the file alone: it has no record, written outside the server
	MetadataFolder   = "folder"   // a directory listed as "<dir>/": empty, or created as a marker
)

// InventoryEntry is one object as Inventory reports it. For an edited or missing entry
// the ETag is the synthetic one listings show, and the checksum and content type are empty.
type InventoryEntry struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"last_modified"`
	ETag        string    `json:"etag"`
	Checksum    string    `json:"checksum,omitempty"` // ALGORITHM:base64, several space-separated
	ContentType string    `json:"content_type,omitempty"`
	Metadata    string    `json:"metadata"`
}

// Inventory passes every object of bucket, or of every bucket when it is empty, whose
// key starts with prefix to fn in S3 order. It only reads, so it may run beside a server;
// an object the server replaces while it is reached is looked at twice, and one deleted
// meanwhile is left out.
func (s *Store) Inventory(ctx context.Context, bucket, prefix string, fn func(InventoryEntry) error) error {
	buckets := []string{bucket}
	if bucket == "" {
		all, err := s.ListBuckets()
		if err != nil {
			return err
		}
		buckets = buckets[:0]
		for _, b := range all {
			buckets = append(buckets, b.Name)
		}
	} else if err := s.requireBucket(bucket); err != nil {
		return err
	}
	for _, b := range buckets {
		var ferr error
		err := s.walkKeys(b, ListParams{Prefix: prefix}, func(e listEntry) bool {
			if ferr = ctx.Err(); ferr == nil {
				if ie, ok := s.inventoryEntry(b, e); ok {
					ferr = fn(ie)
				}
			}
			return ferr == nil
		})
		if err := cmp.Or(ferr, err); err != nil {
			return err
		}
	}
	return nil
}

// inventoryEntry describes e, or reports false for an object deleted since the walk saw it.
func (s *Store) inventoryEntry(bucket string, e listEntry) (InventoryEntry, bool) {
	ie := InventoryEntry{Bucket: bucket, Key: e.key, ModTime: e.fi.ModTime().UTC()}
	if e.marker {
		ie.ETag, ie.Metadata = emptyETag, MetadataFolder
		return ie, true
	}
	fi := e.fi
	m, ok := s.readSidecar(bucket, e.key)
	if !ok || !m.matches(fi) {
		// The server places the data before its record and removes it before its record, so
		// a write or delete in between looks like an outside edit: look again.
		cur, err := s.r.Stat(s.rel(s.dataPath(bucket, e.key)))
		if err != nil {
			return InventoryEntry{}, false
		}
		fi = cur
		m, ok = s.readSidecar(bucket, e.key)
	}
	ie.Size, ie.ModTime = fi.Size(), fi.ModTime().UTC()
	switch {
	case !ok:
		ie.ETag, ie.Metadata = syntheticETag(fi), MetadataMissing
	case !m.matches(fi):
		ie.ETag, ie.Metadata = syntheticETag(fi), MetadataEdited
	default:
		ie.ETag, ie.Metadata, ie.ContentType = m.ETag, MetadataRecorded, m.ContentType
		var sums []string
		for _, alg := range slices.Sorted(maps.Keys(m.Checksums)) {
			sums = append(sums, alg+":"+m.Checksums[alg])
		}
		ie.Checksum = strings.Join(sums, " ")
	}
	return ie, true
}
