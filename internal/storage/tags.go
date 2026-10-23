package storage

import (
	"encoding/json"
	"os"
)

// SetTags replaces the tags of bucket/key; nil removes them. Only the sidecar changes,
// and its modification time is kept: tagging an object does not count as using it.
func (s *Store) SetTags(bucket, key string, tags []Tag) error {
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	unlock := s.lock(bucket, key)
	defer unlock()
	if err := s.requireBucket(bucket); err != nil {
		return err
	}
	if checkKey(key) != nil {
		return ErrNoSuchKey
	}
	m, ok := s.readSidecar(bucket, key)
	if isDirKey(key) {
		// A directory that only holds other keys is not an object; a sidecar would make it one.
		if fi, err := s.r.Stat(s.rel(s.dataPath(bucket, key))); err != nil || !fi.IsDir() || !ok || !m.Dir {
			return ErrNoSuchKey
		}
	} else {
		f, fi, err := s.openData(bucket, key)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if !ok || !m.matches(fi) {
			// The key lock is held, so persist here rather than through refresh.
			if m, err = s.refresh(bucket, key, f, fi, m, false); err != nil {
				return err
			}
		}
	}
	m.Tags = tags
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := s.metaPath(bucket, key)
	old, statErr := os.Stat(p)
	if err := s.writeFileAtomic(p, raw); err != nil {
		return err
	}
	if statErr == nil {
		_ = os.Chtimes(p, old.ModTime(), old.ModTime())
	}
	return nil
}
