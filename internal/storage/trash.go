package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Each trashed object is a directory SysDir/trash/<bucket>/<id> holding the data file and
// a manifest, so a restore puts back the object's metadata with it.
const (
	trashData     = "data"
	trashManifest = "trash.json"
)

// ErrNoSuchTrash reports a trash entry that does not exist, was restored or expired.
var ErrNoSuchTrash = errors.New("no such trash entry")

var trashIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TrashEntry is an object deleted from a bucket with trash-after set.
type TrashEntry struct {
	ID        string    `json:"id"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deleted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type trashManifestFile struct {
	TrashEntry
	Sidecar *sidecar `json:"sidecar,omitempty"` // nil when the object had no trusted sidecar
}

// SetTrash sets how long each bucket keeps deleted objects; a bucket not in the map
// deletes them at once. Safe to call while serving.
func (s *Store) SetTrash(after map[string]time.Duration) { s.trashAfter.Store(&after) }

// TrashAfter returns how long bucket keeps deleted objects, 0 when it does not.
func (s *Store) TrashAfter(bucket string) time.Duration {
	if m := s.trashAfter.Load(); m != nil {
		return (*m)[bucket]
	}
	return 0
}

// trashDir is SysDir/trash/<bucket>[/<id>]; bucket must be a valid name.
func (s *Store) trashDir(bucket string, id ...string) string {
	return s.sys(append([]string{"trash", bucket}, id...)...)
}

// trashFile moves the object file p of bucket/key, described by fi, into the trash instead of
// deleting it; removed means what it does for removeFile. The caller holds the key lock.
func (s *Store) trashFile(bucket, key, p string, fi os.FileInfo, after time.Duration) (removed bool, err error) {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	dir := s.trashDir(bucket, id)
	if err := os.MkdirAll(s.trashDir(bucket), sysDirPerm); err != nil {
		return false, err
	}
	if err := os.Mkdir(dir, sysDirPerm); err != nil {
		return false, err
	}
	now := s.now()
	man := trashManifestFile{TrashEntry: TrashEntry{ID: id, Bucket: bucket, Key: key, Size: fi.Size(), DeletedAt: now, ExpiresAt: now.Add(after)}}
	if m, ok := s.readSidecar(bucket, key); ok && m.matches(fi) {
		man.Sidecar = &m
	}
	out, err := json.Marshal(man)
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, trashManifest), out, 0o600)
	}
	if err == nil {
		err = rename(s.r, s.rel(p), s.rel(filepath.Join(dir, trashData)))
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_ = os.Remove(s.metaPath(bucket, key))
	s.adjust(bucket, -1, -fi.Size())
	s.pruneEmptyParents(bucket, filepath.Dir(p))
	return true, nil
}

func readTrash(dir string) (trashManifestFile, error) {
	var man trashManifestFile
	raw, err := os.ReadFile(filepath.Join(dir, trashManifest))
	if err == nil {
		err = json.Unmarshal(raw, &man)
	}
	return man, err
}

// restorable reports an entry of bucket that has not expired and whose data arrived:
// a delete interrupted between writing the manifest and moving the file leaves none.
func (s *Store) restorable(dir, bucket string, man trashManifestFile) bool {
	if man.Bucket != bucket || man.ID != filepath.Base(dir) || checkKey(man.Key) != nil || isDirKey(man.Key) ||
		!s.now().Before(man.ExpiresAt) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, trashData))
	return err == nil
}

// ListTrash returns the trashed objects of bucket, most recently deleted first.
func (s *Store) ListTrash(bucket string) ([]TrashEntry, error) {
	out := []TrashEntry{}
	if !bucketNameRe.MatchString(bucket) {
		return out, nil
	}
	ents, err := os.ReadDir(s.trashDir(bucket))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		dir := s.trashDir(bucket, e.Name())
		man, err := readTrash(dir)
		if err != nil || !s.restorable(dir, bucket, man) {
			continue
		}
		out = append(out, man.TrashEntry)
	}
	slices.SortFunc(out, func(a, b TrashEntry) int {
		if c := b.DeletedAt.Compare(a.DeletedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

// trashEntry loads entry id of bucket. The caller holds trashMu.
func (s *Store) trashEntry(bucket, id string) (trashManifestFile, string, error) {
	if !trashIDRe.MatchString(id) || !bucketNameRe.MatchString(bucket) {
		return trashManifestFile{}, "", ErrNoSuchTrash
	}
	dir := s.trashDir(bucket, id)
	man, err := readTrash(dir)
	if err != nil || !s.restorable(dir, bucket, man) {
		return trashManifestFile{}, "", ErrNoSuchTrash
	}
	return man, dir, nil
}

// RestoreTrash puts entry id back into bucket under its key. It never replaces an
// object: one at that key since fails it with ErrPreconditionFailed.
func (s *Store) RestoreTrash(bucket, id string) (ObjectInfo, error) {
	info, over, err := s.restoreTrash(bucket, id)
	if err != nil {
		return ObjectInfo{}, err
	}
	if info.ETag == "" {
		// Restored without a trusted sidecar: a read records one and makes now its last
		// use, so expiry and eviction do not take it for an object unused since its mtime.
		if hi, err := s.HeadObject(bucket, info.Key); err == nil {
			info = hi
		}
	}
	if over {
		s.wake()
	}
	return info, nil
}

// restoreTrash does RestoreTrash's work under the locks; HeadObject must run after them.
func (s *Store) restoreTrash(bucket, id string) (ObjectInfo, bool, error) {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	man, dir, err := s.trashEntry(bucket, id)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	bl := s.bucketLock(bucket)
	bl.RLock()
	defer bl.RUnlock()
	if err := s.requireBucket(bucket); err != nil {
		return ObjectInfo{}, false, err
	}
	unlock := s.lock(bucket, man.Key)
	defer unlock()

	dst := s.dataPath(bucket, man.Key)
	if fi, err := s.r.Lstat(s.rel(dst)); err == nil {
		if fi.IsDir() {
			return ObjectInfo{}, false, ErrConflict
		}
		return ObjectInfo{}, false, ErrPreconditionFailed
	} else if errors.Is(err, syscall.ENOTDIR) {
		return ObjectInfo{}, false, ErrConflict
	}
	src := filepath.Join(dir, trashData)
	fi, err := os.Stat(src)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	over, err := s.account(bucket, 1, fi.Size())
	if err != nil {
		return ObjectInfo{}, false, err
	}
	// The sidecar first: a rename keeps size and mtime, so it matches the data once that
	// lands, and a crash in between leaves an orphan the sweep removes, not a bare object.
	info := ObjectInfo{Key: man.Key, Size: fi.Size(), ModTime: fi.ModTime()}
	meta := s.metaPath(bucket, man.Key)
	wrote := false
	if m := man.Sidecar; m != nil && m.matches(fi) {
		if raw, err := json.Marshal(m); err == nil && s.writeFileAtomic(meta, raw) == nil {
			info, wrote = m.info(fi.ModTime()), true
		}
	}
	if err := s.place(src, dst, dirPerm); err != nil {
		if wrote {
			_ = os.Remove(meta)
		}
		s.adjust(bucket, -1, -fi.Size())
		return ObjectInfo{}, false, err
	}
	_ = os.RemoveAll(dir)
	return info, over, nil
}

// PurgeTrash deletes entry id of bucket for good.
func (s *Store) PurgeTrash(bucket, id string) error {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	_, dir, err := s.trashEntry(bucket, id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
