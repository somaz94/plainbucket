package storage

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ListParams select the keys of a listing.
type ListParams struct {
	Prefix    string
	Delimiter string
	After     string // exclusive start: start-after, marker or continuation token
	MaxKeys   int
}

// ListResult is one page of a listing. Next is the last key or common prefix returned,
// which resumes the listing when passed back as After.
type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	Truncated      bool
	Next           string
}

type listEntry struct {
	key    string
	fi     os.FileInfo
	marker bool // an empty directory, listed as the key "<dir>/"
}

// List returns keys in S3 order: ascending by the UTF-8 bytes of the whole key.
func (s *Store) List(bucket string, p ListParams) (ListResult, error) {
	var res ListResult
	if err := s.requireBucket(bucket); err != nil {
		return res, err
	}
	if p.MaxKeys <= 0 {
		return res, nil
	}
	lastCP := ""
	yield := func(e listEntry) bool {
		if p.Delimiter != "" {
			if cp := commonPrefix(e.key, p.Prefix, p.Delimiter); cp != "" {
				// A token equal to the prefix means the whole group was returned already.
				if cp == lastCP || cp == p.After {
					return true
				}
				if len(res.Objects)+len(res.CommonPrefixes) == p.MaxKeys {
					res.Truncated = true
					return false
				}
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				lastCP, res.Next = cp, cp
				return true
			}
		}
		if len(res.Objects)+len(res.CommonPrefixes) == p.MaxKeys {
			res.Truncated = true
			return false
		}
		res.Objects = append(res.Objects, s.listInfo(bucket, e))
		res.Next = e.key
		return true
	}

	startKP := p.Prefix[:strings.LastIndex(p.Prefix, "/")+1]
	if startKP != "" && checkKey(startKP) != nil {
		return res, nil // no stored key has an empty, "." or ".." segment
	}
	w := walker{s: s, bucket: bucket, p: p, yield: yield}
	start := filepath.Join(s.bucketPath(bucket), filepath.FromSlash(startKP))
	if _, err := w.walk(start, startKP); err != nil {
		return ListResult{}, err
	}
	return res, nil
}

// commonPrefix returns the prefix key rolls up to under delimiter, or "".
func commonPrefix(key, prefix, delimiter string) string {
	rest := key[len(prefix):]
	i := strings.Index(rest, delimiter)
	if i < 0 {
		return ""
	}
	return prefix + rest[:i+len(delimiter)]
}

type dirItem struct {
	sortKey string // the name, plus "/" for a directory
	name    string
	fi      os.FileInfo
	isDir   bool
}

type walker struct {
	s      *Store
	bucket string
	p      ListParams
	yield  func(listEntry) bool
}

// walk yields the keys under dir, whose key prefix is kp, in S3 order, and reports
// whether to continue. os.ReadDir order is not enough: "a-c" sorts before "a/b", but a
// walk would visit directory "a" before the file "a-c". A directory that cannot be read
// is an error, not an empty page: a sync client would delete what it did not see.
func (w *walker) walk(dir, kp string) (bool, error) {
	p := w.p
	ents, err := w.readDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || escapesRoot(err) {
			return true, nil
		}
		return false, err
	}
	// The directory itself is listed as "<dir>/" when it is empty or an explicit marker.
	if kp != "" && kp > p.After && strings.HasPrefix(kp, p.Prefix) && (len(ents) == 0 || w.s.hasMarker(w.bucket, kp)) {
		if fi, err := w.s.r.Stat(w.s.rel(dir)); err == nil && !w.yield(listEntry{key: kp, fi: fi, marker: true}) {
			return false, nil
		}
	}

	items := make([]dirItem, 0, len(ents))
	for _, e := range ents {
		it, ok := w.item(dir, e)
		if ok {
			items = append(items, it)
		}
	}
	slices.SortFunc(items, func(a, b dirItem) int { return strings.Compare(a.sortKey, b.sortKey) })

	for _, it := range items {
		key := kp + it.sortKey
		if !it.isDir {
			if strings.HasPrefix(key, p.Prefix) && key > p.After && !w.yield(listEntry{key: key, fi: it.fi}) {
				return false, nil
			}
			continue
		}
		// Skip a subtree that cannot match the prefix or lies wholly at or before After.
		if !strings.HasPrefix(key, p.Prefix) && !strings.HasPrefix(p.Prefix, key) {
			continue
		}
		if p.After >= key && !strings.HasPrefix(p.After, key) {
			continue
		}
		// Every key below rolls up to the same common prefix: emit it without descending.
		if p.Delimiter != "" && strings.HasPrefix(key, p.Prefix) && !strings.HasPrefix(p.After, key) {
			if cp := commonPrefix(key, p.Prefix, p.Delimiter); cp != "" {
				if !w.yield(listEntry{key: key, fi: it.fi, marker: true}) {
					return false, nil
				}
				continue
			}
		}
		if more, err := w.walk(filepath.Join(dir, it.name), key); !more || err != nil {
			return more, err
		}
	}
	return true, nil
}

func (w *walker) readDir(dir string) ([]os.DirEntry, error) {
	f, err := w.s.r.Open(w.s.rel(dir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadDir(-1)
}

// item classifies a directory entry. Symlinks to files inside the root are objects;
// symlinks to directories, outside the root, or dangling are skipped.
func (w *walker) item(dir string, e os.DirEntry) (dirItem, bool) {
	name := e.Name()
	switch {
	case e.Type()&os.ModeSymlink != 0:
		fi, err := w.s.r.Stat(w.s.rel(filepath.Join(dir, name)))
		if err != nil || !fi.Mode().IsRegular() {
			return dirItem{}, false
		}
		return dirItem{name, name, fi, false}, true
	case e.IsDir():
		fi, err := e.Info()
		if err != nil {
			return dirItem{}, false
		}
		return dirItem{name + "/", name, fi, true}, true
	case e.Type().IsRegular():
		fi, err := e.Info()
		if err != nil {
			return dirItem{}, false
		}
		return dirItem{name, name, fi, false}, true
	}
	return dirItem{}, false
}

func (s *Store) listInfo(bucket string, e listEntry) ObjectInfo {
	if e.marker {
		return ObjectInfo{Key: e.key, ModTime: e.fi.ModTime(), ETag: emptyETag, IsDir: true}
	}
	info := ObjectInfo{Key: e.key, Size: e.fi.Size(), ModTime: e.fi.ModTime()}
	if m, ok := s.readSidecar(bucket, e.key); ok && m.matches(e.fi) {
		info.ETag = m.ETag
	} else {
		info.ETag = syntheticETag(e.fi)
	}
	return info
}
