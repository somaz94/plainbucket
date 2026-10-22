package storage

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Problems reported by Fsck besides the sweep's leftovers.
const (
	ProblemMetadata = "missing or outdated metadata"
	ProblemName     = "file name that is not a valid key"
)

// Finding is one problem Fsck found. Path is a bucket/key for objects and a file path
// for system entries. Fixed reports whether repair resolved it.
type Finding struct {
	Kind  string
	Path  string
	Fixed bool
}

// Fsck checks the root while no server runs on it (see Lock): objects whose metadata is
// missing or no longer matches the file, files whose names cannot be keys, and the
// leftovers the janitor removes. With repair it rebuilds the metadata and removes the
// leftovers; files with invalid names are only reported.
func (s *Store) Fsck(ctx context.Context, uploadExpiry time.Duration, repair bool, report func(Finding)) error {
	buckets, err := s.ListBuckets()
	if err != nil {
		return err
	}
	for _, b := range buckets {
		root := s.bucketPath(b.Name)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			key := filepath.ToSlash(rel)
			name := b.Name + "/" + key
			if checkKey(key) != nil {
				report(Finding{Kind: ProblemName, Path: name})
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if m, ok := s.readSidecar(b.Name, key); ok && m.matches(fi) {
				return nil
			}
			f := Finding{Kind: ProblemMetadata, Path: name}
			if repair {
				_, err := s.HeadObject(b.Name, key)
				f.Fixed = err == nil
			}
			report(f)
			return nil
		})
		if err != nil {
			return err
		}
	}
	s.sweep(ctx, uploadExpiry, func(kind, path string) bool {
		f := Finding{Kind: kind, Path: path}
		if repair {
			f.Fixed = os.RemoveAll(path) == nil
		}
		report(f)
		return f.Fixed
	})
	return ctx.Err()
}
