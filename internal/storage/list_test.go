package storage

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// reference lists keys the way S3 defines it, by brute force over a sorted key set.
func reference(keys []string, p ListParams) (objs, cps []string) {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	seen := map[string]bool{}
	for _, k := range sorted {
		if !strings.HasPrefix(k, p.Prefix) || k <= p.After {
			continue
		}
		if p.Delimiter != "" {
			if cp := commonPrefix(k, p.Prefix, p.Delimiter); cp != "" {
				if !seen[cp] && cp != p.After {
					seen[cp] = true
					cps = append(cps, cp)
					objs = append(objs, cp) // keep the merged order in objs
				}
				continue
			}
		}
		objs = append(objs, k)
	}
	return objs, cps
}

// listAll pages through List with the given page size and returns the merged order.
func listAll(t *testing.T, s *Store, p ListParams, page int) []string {
	t.Helper()
	var out []string
	p.MaxKeys = page
	for range 10000 {
		res, err := s.List("bkt", p)
		if err != nil {
			t.Fatal(err)
		}
		// Merge objects and prefixes back into S3 order.
		merged := make([]string, 0, len(res.Objects)+len(res.CommonPrefixes))
		for _, o := range res.Objects {
			merged = append(merged, o.Key)
		}
		merged = append(merged, res.CommonPrefixes...)
		slices.Sort(merged)
		out = append(out, merged...)
		if !res.Truncated {
			return out
		}
		if len(merged) != page {
			t.Fatalf("truncated page has %d items, want %d", len(merged), page)
		}
		p.After = res.Next
	}
	t.Fatal("listing did not terminate")
	return nil
}

func TestListOrderAgainstReference(t *testing.T) {
	s := newStore(t)
	// Names chosen to straddle '/' (0x2f) in byte order: '-' and '.' sort before it, '0' and 'a' after.
	keys := []string{
		"a", "a-c", "a.b", "a0", "b/x", "b-y", "b/y/z", "b/y-z", "b/y.z", "c/d/e/f",
		"c/d-e", "c/d/e-f", "c/d0", "dir/", "dir2/sub/", "z", "ü/é", "space key", "b/y/zz",
	}
	for _, k := range keys {
		put(t, s, k, "")
	}
	params := []ListParams{
		{},
		{Prefix: "b"},
		{Prefix: "b/"},
		{Prefix: "b/y"},
		{Delimiter: "/"},
		{Prefix: "b/", Delimiter: "/"},
		{Prefix: "c/d", Delimiter: "/"},
		{Delimiter: "-"},
		{Prefix: "b/", Delimiter: "y/"},
		{After: "b/y"},
		{After: "b/y/z", Delimiter: "/"},
		{After: "b/", Delimiter: "/"},
		{Prefix: "dir"},
		{Prefix: "nope/"},
		{Prefix: "a/x"},
	}
	for _, p := range params {
		want, _ := reference(keys, p)
		for _, page := range []int{1, 2, 3, 1000} {
			got := listAll(t, s, p, page)
			if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Errorf("%+v page %d:\n got %q\nwant %q", p, page, got, want)
			}
		}
	}
}

func TestListRandomized(t *testing.T) {
	s := newStore(t)
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"a", "b", "a-", "a.", "a0", "x"}
	set := map[string]bool{}
	for len(set) < 150 {
		depth := 1 + r.IntN(4)
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = alphabet[r.IntN(len(alphabet))]
		}
		set[strings.Join(parts, "/")] = true
	}
	var keys []string
	for k := range set {
		if _, err := s.PutObject("bkt", k, strings.NewReader(""), 0, PutOptions{}); err == nil {
			keys = append(keys, k)
		}
	}
	for i := range 40 {
		p := ListParams{}
		if i%2 == 0 {
			p.Delimiter = "/"
		}
		if i%3 == 0 {
			p.Prefix = keys[r.IntN(len(keys))][:1]
		}
		if i%5 == 0 {
			p.After = keys[r.IntN(len(keys))]
		}
		want, _ := reference(keys, p)
		got := listAll(t, s, p, 1+r.IntN(7))
		if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("%+v:\n got %q\nwant %q", p, got, want)
		}
	}
}

func TestListDetails(t *testing.T) {
	s := newStore(t)
	info := put(t, s, "f.txt", "hello")
	put(t, s, "marker/", "")
	res, err := s.List("bkt", ListParams{MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 2 || res.Truncated {
		t.Fatalf("res = %+v", res)
	}
	if o := res.Objects[0]; o.Key != "f.txt" || o.Size != 5 || o.ETag != info.ETag {
		t.Errorf("object = %+v", o)
	}
	if o := res.Objects[1]; o.Key != "marker/" || !o.IsDir || o.ETag != emptyETag {
		t.Errorf("marker = %+v", o)
	}
	if res, _ := s.List("bkt", ListParams{MaxKeys: 0}); len(res.Objects) != 0 || res.Truncated {
		t.Errorf("max-keys 0: %+v", res)
	}
	if _, err := s.List("nope", ListParams{MaxKeys: 1}); err != ErrNoSuchBucket {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestListSymlinks(t *testing.T) {
	s := newStore(t)
	put(t, s, "real/f", "x")
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	b := filepath.Join(s.Root(), "bkt")
	links := map[string]string{
		"loop":     "real",                               // directory: skipped
		"link":     filepath.Join("real", "f"),           // relative file link inside the root: listed
		"absolute": filepath.Join(b, "real", "f"),        // absolute target: os.Root refuses it
		"escape":   filepath.Join(outside, "secret"),     // outside the root
		"dotdot":   filepath.Join("..", "..", "..", "x"), // climbs out of the root
		"dangling": "gone",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(b, name)); err != nil {
			t.Skip(err)
		}
	}
	got := listAll(t, s, ListParams{}, 100)
	if want := []string{"link", "real/f"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, _, err := s.GetObject("bkt", "escape"); err != ErrNoSuchKey {
		t.Errorf("GET through an escaping link: %v", err)
	}
	if _, _, err := s.GetObject("bkt", "loop/f"); err != nil {
		t.Errorf("GET through a directory link inside the root: %v", err)
	}
}

func TestListPrefixCannotEscape(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "x")
	_ = s.CreateBucket("other")
	_, _ = s.PutObject("other", "secret", strings.NewReader("s"), 1, PutOptions{})
	for _, prefix := range []string{"../", "../other/", "a/../../", "./", "a//"} {
		res, err := s.List("bkt", ListParams{Prefix: prefix, MaxKeys: 100})
		if err != nil || len(res.Objects)+len(res.CommonPrefixes) != 0 {
			t.Errorf("prefix %q: %+v %v", prefix, res, err)
		}
	}
}

func TestListUnreadableDirectoryFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	s := newStore(t)
	put(t, s, "locked/f", "x")
	dir := filepath.Join(s.Root(), "bkt", "locked")
	_ = os.Chmod(dir, 0)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := s.List("bkt", ListParams{MaxKeys: 10}); err == nil {
		t.Error("an unreadable directory produced a silently short listing")
	}
}

func BenchmarkList(b *testing.B) {
	s, _ := New(b.TempDir())
	_ = s.CreateBucket("bkt")
	for i := range 2000 {
		k := fmt.Sprintf("d%02d/f%04d", i%50, i)
		_, _ = s.PutObject("bkt", k, strings.NewReader(""), 0, PutOptions{})
	}
	for b.Loop() {
		_, _ = s.List("bkt", ListParams{MaxKeys: 1000})
	}
}
