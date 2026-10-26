package storage

import (
	"cmp"
	"container/heap"
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
	start, err := w.open(dirRef{path: s.rel(s.bucketPath(bucket))}, filepath.FromSlash(strings.TrimSuffix(startKP, "/")), 0)
	if err != nil {
		if gone(err) {
			return res, nil
		}
		return ListResult{}, err
	}
	defer start.close()
	if _, err := w.walk(start, startKP); err != nil {
		return ListResult{}, err
	}
	return res, nil
}

// gone reports an error that means there is nothing to list, not a failure to list.
func gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || escapesRoot(err)
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
	fi      os.FileInfo // a symlink's target
	isDir   bool
}

type walker struct {
	s      *Store
	bucket string
	p      ListParams
	yield  func(listEntry) bool
}

// heldRoots is how deep a walk keeps a Root open on each directory, which makes lookups
// one path component each. Deeper directories go through the store's root, so a listing
// holds a bounded number of descriptors however deep its keys run.
const heldRoots = 32

// dirRef is a directory being walked: path in the store's root, and a Root opened on it
// unless the walk is deeper than heldRoots.
type dirRef struct {
	root  *os.Root
	path  string
	depth int
}

func (d dirRef) close() {
	if d.root != nil {
		_ = d.root.Close()
	}
}

// open returns the subdirectory name of d (d itself for ""). OpenRoot reports a
// non-directory with an error of its own, not ENOTDIR, so it is told apart afterwards.
func (w *walker) open(d dirRef, name string, depth int) (dirRef, error) {
	sub := dirRef{path: filepath.Join(d.path, name), depth: depth}
	if depth > heldRoots {
		return sub, nil
	}
	var err error
	if d.root != nil {
		sub.root, err = d.root.OpenRoot(cmp.Or(name, "."))
	} else {
		sub.root, err = w.s.r.OpenRoot(sub.path)
	}
	if err != nil && !gone(err) {
		if fi, serr := w.s.r.Stat(sub.path); serr != nil || !fi.IsDir() {
			return sub, os.ErrNotExist
		}
	}
	return sub, err
}

func (w *walker) lstat(d dirRef, name string) (os.FileInfo, error) {
	if d.root != nil {
		return d.root.Lstat(name)
	}
	return w.s.r.Lstat(filepath.Join(d.path, name))
}

func (w *walker) readNames(d dirRef) ([]string, error) {
	var f *os.File
	var err error
	if d.root != nil {
		f, err = d.root.Open(".")
	} else {
		f, err = w.s.r.Open(d.path)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// walk yields the keys under dir, whose key prefix is kp, in S3 order, and reports
// whether to continue. A directory that cannot be read is an error, not an empty page: a
// sync client would delete what it did not see.
//
// It reads names only and stats an entry once the walk reaches it, so a page of a large
// directory stats about as many entries as it returns. (ReadDir on a directory opened in
// a Root stats every entry.) Sorting by name is not quite S3 order, in which a directory
// counts as "name/": "a" < "a!" but "a/" > "a!", so directory "a" must come after file
// "a!". A directory therefore waits in a heap until no name still to come can sort
// before it.
func (w *walker) walk(dir dirRef, kp string) (bool, error) {
	p := w.p
	names, err := w.readNames(dir)
	if err != nil {
		if gone(err) {
			return true, nil
		}
		return false, err
	}
	// The directory itself is listed as "<dir>/" when it is empty or an explicit marker.
	if kp != "" && kp > p.After && strings.HasPrefix(kp, p.Prefix) && (len(names) == 0 || w.s.hasMarker(w.bucket, kp)) {
		if fi, err := w.s.r.Stat(dir.path); err == nil && !w.yield(listEntry{key: kp, fi: fi, marker: true}) {
			return false, nil
		}
	}
	names = slices.DeleteFunc(names, func(n string) bool { return !w.mayMatch(kp+n, false) && !w.mayMatch(kp+n+"/", true) })
	slices.Sort(names)

	var waiting dirHeap
	for _, n := range names {
		// Every name from here on sorts at or after n, so a directory before it is next.
		for len(waiting) > 0 && waiting[0].sortKey < n {
			if more, err := w.visit(dir, kp, heap.Pop(&waiting).(dirItem)); !more || err != nil {
				return more, err
			}
		}
		it, ok, err := w.item(dir, n)
		switch {
		case err != nil:
			return false, err
		case !ok:
		case it.isDir:
			heap.Push(&waiting, it)
		default:
			if more, err := w.visit(dir, kp, it); !more || err != nil {
				return more, err
			}
		}
	}
	for len(waiting) > 0 {
		if more, err := w.visit(dir, kp, heap.Pop(&waiting).(dirItem)); !more || err != nil {
			return more, err
		}
	}
	return true, nil
}

// mayMatch reports whether key, a file's or (dir) a directory's, can hold anything the
// listing returns, judged by name alone.
func (w *walker) mayMatch(key string, dir bool) bool {
	p := w.p
	if !dir {
		return strings.HasPrefix(key, p.Prefix) && key > p.After
	}
	if !strings.HasPrefix(key, p.Prefix) && !strings.HasPrefix(p.Prefix, key) {
		return false
	}
	return p.After < key || strings.HasPrefix(p.After, key)
}

// visit yields a file, or rolls up or descends into a directory.
func (w *walker) visit(dir dirRef, kp string, it dirItem) (bool, error) {
	p := w.p
	key := kp + it.sortKey
	if !it.isDir {
		if !w.mayMatch(key, false) {
			return true, nil
		}
		return w.yield(listEntry{key: key, fi: it.fi}), nil
	}
	if !w.mayMatch(key, true) {
		return true, nil
	}
	// Every key below rolls up to the same common prefix: emit it without descending.
	if p.Delimiter != "" && strings.HasPrefix(key, p.Prefix) && !strings.HasPrefix(p.After, key) {
		if cp := commonPrefix(key, p.Prefix, p.Delimiter); cp != "" {
			return w.yield(listEntry{key: key, fi: it.fi, marker: true}), nil
		}
	}
	sub, err := w.open(dir, it.name, dir.depth+1)
	if err != nil {
		if gone(err) {
			return true, nil
		}
		return false, err
	}
	defer sub.close()
	return w.walk(sub, key)
}

// item stats one entry of dir. Symlinks to files inside the root are objects; symlinks to
// directories, outside the root, or dangling are skipped, as are entries gone since the
// directory was read. Any other failure is an error, as for an unreadable directory.
func (w *walker) item(dir dirRef, name string) (dirItem, bool, error) {
	fi, err := w.lstat(dir, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dirItem{}, false, nil
		}
		return dirItem{}, false, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		// Followed in the store's root, as reads do: a link may point above dir and still stay inside.
		if fi, err = w.s.r.Stat(filepath.Join(dir.path, name)); err != nil || !fi.Mode().IsRegular() {
			return dirItem{}, false, nil
		}
		return dirItem{name, name, fi, false}, true, nil
	case fi.IsDir():
		return dirItem{name + "/", name, fi, true}, true, nil
	case fi.Mode().IsRegular():
		return dirItem{name, name, fi, false}, true, nil
	}
	return dirItem{}, false, nil
}

// dirHeap orders the directories waiting in walk by sort key.
type dirHeap []dirItem

func (h dirHeap) Len() int           { return len(h) }
func (h dirHeap) Less(i, j int) bool { return h[i].sortKey < h[j].sortKey }
func (h dirHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dirHeap) Push(x any)        { *h = append(*h, x.(dirItem)) }
func (h *dirHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
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
