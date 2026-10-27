package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func md5sum(s string) []byte {
	h := md5.Sum([]byte(s))
	return h[:]
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *Store, key, body string) ObjectInfo {
	t.Helper()
	info, err := s.PutObject("bkt", key, strings.NewReader(body), int64(len(body)), PutOptions{})
	if err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
	return info
}

func read(t *testing.T, s *Store, key string) (string, ObjectInfo) {
	t.Helper()
	f, info, err := s.GetObject("bkt", key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if f == nil {
		return "", info
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b), info
}

func TestPutGetIsPlainFile(t *testing.T) {
	s := newStore(t)
	info, err := s.PutObject("bkt", "dir/sub/hello.txt", strings.NewReader("hello"), 5, PutOptions{
		Metadata: Metadata{ContentType: "text/plain", UserMeta: map[string]string{"a": "1"}, Headers: map[string]string{"Cache-Control": "no-cache"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ETag != `"5d41402abc4b2a76b9719d911017c592"` || info.Size != 5 {
		t.Errorf("info = %+v", info)
	}
	raw, err := os.ReadFile(filepath.Join(s.Root(), "bkt", "dir", "sub", "hello.txt"))
	if err != nil || string(raw) != "hello" {
		t.Fatalf("plain file = %q, %v", raw, err)
	}
	fi, _ := os.Stat(filepath.Join(s.Root(), "bkt", "dir", "sub", "hello.txt"))
	if fi.Mode().Perm() != filePerm {
		t.Errorf("mode = %v", fi.Mode())
	}
	body, got := read(t, s, "dir/sub/hello.txt")
	if body != "hello" || got.ETag != info.ETag || got.ContentType != "text/plain" ||
		got.UserMeta["a"] != "1" || got.Headers["Cache-Control"] != "no-cache" {
		t.Errorf("got %q %+v", body, got)
	}
}

func TestPutShortBodyAndVerify(t *testing.T) {
	s := newStore(t)
	if _, err := s.PutObject("bkt", "k", strings.NewReader("abc"), 5, PutOptions{}); !errors.Is(err, ErrShortBody) {
		t.Errorf("short body: %v", err)
	}
	boom := errors.New("boom")
	_, err := s.PutObject("bkt", "k", strings.NewReader("abc"), 3, PutOptions{Verify: func([]byte) (map[string]string, error) { return nil, boom }})
	if !errors.Is(err, boom) {
		t.Errorf("verify error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "k")); !os.IsNotExist(err) {
		t.Error("failed upload left a file")
	}
	info, err := s.PutObject("bkt", "k", strings.NewReader("abc"), 3, PutOptions{Verify: func([]byte) (map[string]string, error) {
		return map[string]string{"CRC32": "x"}, nil
	}})
	if err != nil || info.Checksums["CRC32"] != "x" {
		t.Errorf("checksums: %+v %v", info, err)
	}
	if ents, _ := os.ReadDir(s.sys("tmp")); len(ents) != 0 {
		t.Errorf("tmp not cleaned: %d entries", len(ents))
	}
}

func TestKeyValidation(t *testing.T) {
	s := newStore(t)
	cases := map[string]error{
		"":                               ErrInvalidKey,
		"/":                              ErrInvalidKey,
		"a//b":                           ErrInvalidKey,
		"a/./b":                          ErrInvalidKey,
		"../x":                           ErrInvalidKey,
		"nul\x00":                        ErrInvalidKey,
		strings.Repeat("a", 256):         ErrKeyTooLong,
		strings.Repeat("a/", 600):        ErrKeyTooLong,
		"ok/" + strings.Repeat("b", 255): nil,
	}
	for key, want := range cases {
		_, err := s.PutObject("bkt", key, strings.NewReader(""), 0, PutOptions{})
		if !errors.Is(err, want) {
			t.Errorf("key %.20q: %v, want %v", key, err, want)
		}
	}
	if _, err := s.PutObject("nope", "k", strings.NewReader(""), 0, PutOptions{}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
	if _, err := s.PutObject("Bad_Name", "k", strings.NewReader(""), 0, PutOptions{}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("invalid bucket: %v", err)
	}
}

func TestConflicts(t *testing.T) {
	s := newStore(t)
	put(t, s, "a", "file")
	if _, err := s.PutObject("bkt", "a/b", strings.NewReader("x"), 1, PutOptions{}); !errors.Is(err, ErrConflict) {
		t.Errorf("child of a file: %v", err)
	}
	put(t, s, "d/x", "x")
	if _, err := s.PutObject("bkt", "d", strings.NewReader("x"), 1, PutOptions{}); !errors.Is(err, ErrConflict) {
		t.Errorf("file over a directory: %v", err)
	}
	if _, err := s.PutObject("bkt", "a/", strings.NewReader(""), 0, PutOptions{}); !errors.Is(err, ErrConflict) {
		t.Errorf("marker over a file: %v", err)
	}
}

func TestConditions(t *testing.T) {
	s := newStore(t)
	first := put(t, s, "lock", "1")
	_, err := s.PutObject("bkt", "lock", strings.NewReader("2"), 1, PutOptions{Cond: Conditions{IfNoneMatch: "*"}})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("If-None-Match on existing: %v", err)
	}
	if _, err := s.PutObject("bkt", "new", strings.NewReader("2"), 1, PutOptions{Cond: Conditions{IfNoneMatch: "*"}}); err != nil {
		t.Errorf("If-None-Match on new: %v", err)
	}
	if _, err := s.PutObject("bkt", "lock", strings.NewReader("2"), 1, PutOptions{Cond: Conditions{IfNoneMatch: first.ETag}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("If-None-Match with the current ETag: %v", err)
	}
	if _, err := s.PutObject("bkt", "new", strings.NewReader("3"), 1, PutOptions{Cond: Conditions{IfNoneMatch: `"other"`}}); err != nil {
		t.Errorf("If-None-Match with another ETag: %v", err)
	}
	if _, err := s.PutObject("bkt", "lock", strings.NewReader("3"), 1, PutOptions{Cond: Conditions{IfMatch: `"bogus"`}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("If-Match mismatch: %v", err)
	}
	if _, err := s.PutObject("bkt", "lock", strings.NewReader("3"), 1, PutOptions{Cond: Conditions{IfMatch: first.ETag}}); err != nil {
		t.Errorf("If-Match match: %v", err)
	}
	if _, err := s.PutObject("bkt", "absent", strings.NewReader("3"), 1, PutOptions{Cond: Conditions{IfMatch: "*"}}); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("If-Match on missing: %v", err)
	}
}

func TestExternalEditRefreshesSidecar(t *testing.T) {
	s := newStore(t)
	put(t, s, "notes.txt", "v1")
	p := filepath.Join(s.Root(), "bkt", "notes.txt")
	if err := os.WriteFile(p, []byte("version two"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, future, future)

	res, _ := s.List("bkt", ListParams{MaxKeys: 10})
	if !strings.HasSuffix(res.Objects[0].ETag, `-1"`) {
		t.Errorf("stale listing ETag = %s, want synthetic", res.Objects[0].ETag)
	}
	body, info := read(t, s, "notes.txt")
	if body != "version two" || info.ETag != md5ETag(md5sum("version two")) || info.Size != 11 {
		t.Errorf("refreshed %+v", info)
	}
	m, ok := s.readSidecar("bkt", "notes.txt")
	if !ok || !strings.HasPrefix(m.ETag, `"`) || strings.HasSuffix(m.ETag, `-1"`) || m.Size != 11 {
		t.Errorf("sidecar not rewritten: %+v", m)
	}
	res, _ = s.List("bkt", ListParams{MaxKeys: 10})
	if res.Objects[0].ETag != m.ETag {
		t.Errorf("listing ETag %s != %s", res.Objects[0].ETag, m.ETag)
	}
}

func TestExternalFileWithoutSidecar(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(filepath.Join(s.Root(), "bkt", "page.html"), []byte("<p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, info := read(t, s, "page.html")
	if info.ContentType != "text/html; charset=utf-8" || info.ETag == "" {
		t.Errorf("info = %+v", info)
	}
}

func TestDirMarkers(t *testing.T) {
	s := newStore(t)
	if _, err := s.PutObject("bkt", "empty/", strings.NewReader("x"), 1, PutOptions{}); !errors.Is(err, ErrMarkerNotEmpty) {
		t.Errorf("non-empty marker: %v", err)
	}
	info, err := s.PutObject("bkt", "empty/", strings.NewReader(""), 0, PutOptions{})
	if err != nil || !info.IsDir || info.ETag != emptyETag {
		t.Fatalf("marker: %+v %v", info, err)
	}
	if fi, err := os.Stat(filepath.Join(s.Root(), "bkt", "empty")); err != nil || !fi.IsDir() {
		t.Fatal("marker is not a directory")
	}
	f, info, err := s.GetObject("bkt", "empty/")
	if f != nil || err != nil || info.ContentType != DirContentType {
		t.Errorf("get marker: %v %+v", err, info)
	}
	if _, _, err := s.GetObject("bkt", "empty"); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("directory read as object: %v", err)
	}
	if _, _, err := s.GetObject("bkt", "missing/"); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("missing marker: %v", err)
	}
	if err := s.DeleteObject("bkt", "empty/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "empty")); !os.IsNotExist(err) {
		t.Error("marker not removed")
	}
}

// A "<dir>/" key exists exactly when the listing shows it: an explicit marker, or a
// directory made outside the server while it is empty.
func TestDirKeyMatchesListing(t *testing.T) {
	s := newStore(t)
	put(t, s, "implicit/obj", "x")
	if err := os.Mkdir(filepath.Join(s.Root(), "bkt", "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("bkt", "explicit/", strings.NewReader(""), 0, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "explicit/obj", "x")
	check := func(want map[string]bool) {
		t.Helper()
		res, err := s.List("bkt", ListParams{MaxKeys: 100})
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, o := range res.Objects {
			listed[o.Key] = true
		}
		for key, ok := range want {
			_, err := s.HeadObject("bkt", key)
			if (err == nil) != ok || listed[key] != ok {
				t.Errorf("%s: head %v, listed %v, want %v", key, err, listed[key], ok)
			}
		}
	}
	if err := os.Mkdir(filepath.Join(s.Root(), "bkt", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("empty", filepath.Join(s.Root(), "bkt", "link")); err != nil {
		t.Fatal(err)
	}
	check(map[string]bool{"implicit/": false, "outside/": true, "explicit/": true, "link/": false})
	// A marker may be created over a directory that is not one yet.
	if _, err := s.PutObject("bkt", "implicit/", strings.NewReader(""), 0, PutOptions{Cond: Conditions{IfNoneMatch: "*"}}); err != nil {
		t.Errorf("marker over an implicit directory: %v", err)
	}
	check(map[string]bool{"implicit/": true})
	if err := os.WriteFile(filepath.Join(s.Root(), "bkt", "outside", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(map[string]bool{"outside/": false})
}

func TestDeletePrunesAndIgnoresMissing(t *testing.T) {
	s := newStore(t)
	put(t, s, "a/b/c.txt", "x")
	put(t, s, "a/keep.txt", "y")
	if err := s.DeleteObject("bkt", "a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "a", "b")); !os.IsNotExist(err) {
		t.Error("empty parent kept")
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "a")); err != nil {
		t.Error("non-empty parent removed")
	}
	if _, err := os.Stat(s.metaPath("bkt", "a/b/c.txt")); !os.IsNotExist(err) {
		t.Error("sidecar kept")
	}
	for _, k := range []string{"missing", "a", "../bad", "a/b/c.txt"} {
		if err := s.DeleteObject("bkt", k); err != nil {
			t.Errorf("delete %q: %v", k, err)
		}
	}
	if err := s.DeleteObject("nope", "k"); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt")); err != nil {
		t.Error("bucket directory pruned")
	}
}

func TestCopyObject(t *testing.T) {
	s := newStore(t)
	src, err := s.PutObject("bkt", "src", strings.NewReader("data"), 4, PutOptions{Metadata: Metadata{ContentType: "text/plain", UserMeta: map[string]string{"k": "v"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBucket("other"); err != nil {
		t.Fatal(err)
	}
	cp, err := s.CopyObject(context.Background(), "bkt", "src", "other", "dst", CopyOptions{})
	if err != nil || cp.ETag != src.ETag {
		t.Fatalf("copy: %+v %v", cp, err)
	}
	f, got, _ := s.GetObject("other", "dst")
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "data" || got.ContentType != "text/plain" || got.UserMeta["k"] != "v" {
		t.Errorf("copied %q %+v", b, got)
	}

	if _, err := s.CopyObject(context.Background(), "bkt", "src", "bkt", "src", CopyOptions{ReplaceMetadata: true, Metadata: Metadata{ContentType: "application/json"}}); err != nil {
		t.Fatal(err)
	}
	body, self := read(t, s, "src")
	if body != "data" || self.ContentType != "application/json" || self.UserMeta != nil {
		t.Errorf("self copy %q %+v", body, self)
	}

	if _, err := s.CopyObject(context.Background(), "bkt", "src", "bkt", "x", CopyOptions{SrcIfMatch: `"nope"`}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("src if-match: %v", err)
	}
	if _, err := s.CopyObject(context.Background(), "bkt", "src", "bkt", "x", CopyOptions{SrcIfNoneMatch: src.ETag}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("src if-none-match: %v", err)
	}
	if _, err := s.CopyObject(context.Background(), "bkt", "missing", "bkt", "x", CopyOptions{}); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("missing source: %v", err)
	}
	if _, err := s.CopyObject(context.Background(), "bkt", "src", "nope", "x", CopyOptions{}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing destination bucket: %v", err)
	}
	if _, err := s.CopyObject(context.Background(), "bkt", "src", "bkt", "x/", CopyOptions{}); !errors.Is(err, ErrMarkerNotEmpty) {
		t.Errorf("copy onto marker: %v", err)
	}
	put(t, s, "m/", "")
	if info, err := s.CopyObject(context.Background(), "bkt", "m/", "bkt", "n/", CopyOptions{}); err != nil || !info.IsDir {
		t.Errorf("copy marker: %+v %v", info, err)
	}
}

func TestBuckets(t *testing.T) {
	s := newStore(t)
	if err := s.CreateBucket("bkt"); !errors.Is(err, ErrBucketExists) {
		t.Errorf("duplicate: %v", err)
	}
	for _, bad := range []string{"ab", "UPPER", "-x-", "a..b", ".plainbucket", strings.Repeat("a", 64)} {
		if err := s.CreateBucket(bad); !errors.Is(err, ErrInvalidBucketName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A directory made outside the server is a bucket; a file or a bad name is not.
	_ = os.Mkdir(filepath.Join(s.Root(), "imported"), 0o755)
	_ = os.Mkdir(filepath.Join(s.Root(), "Not_A_Bucket"), 0o755)
	_ = os.WriteFile(filepath.Join(s.Root(), "file-not-bucket"), nil, 0o644)
	bs, err := s.ListBuckets()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range bs {
		names = append(names, b.Name)
		if b.Created.IsZero() {
			t.Errorf("%s has no creation time", b.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"bkt", "imported"}) {
		t.Errorf("buckets = %v", names)
	}
	if err := s.HeadBucket("imported"); err != nil {
		t.Error(err)
	}

	put(t, s, "k", "x")
	id, _ := s.CreateUpload("bkt", "up", Metadata{}, MultipartChecksum{})
	if err := s.DeleteBucket("bkt"); !errors.Is(err, ErrBucketNotEmpty) {
		t.Errorf("non-empty: %v", err)
	}
	_ = s.DeleteObject("bkt", "k")
	if err := s.DeleteBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.sys("uploads", id)); !os.IsNotExist(err) {
		t.Error("upload of deleted bucket kept")
	}
	if err := s.DeleteBucket("bkt"); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("deleted twice: %v", err)
	}
}

func TestCaseInsensitiveProbe(t *testing.T) {
	s := newStore(t)
	// Either answer is valid; the probe must not leave files behind.
	_ = s.CaseInsensitive()
	if ents, _ := os.ReadDir(s.sys("tmp")); len(ents) != 0 {
		t.Errorf("probe left %d files", len(ents))
	}
}

func TestPlaceRetriesAndConflicts(t *testing.T) {
	s := newStore(t)
	calls := 0
	orig := rename
	t.Cleanup(func() { rename = orig })
	rename = func(r *os.Root, src, dst string) error {
		calls++
		if calls == 1 {
			return os.ErrNotExist
		}
		return orig(r, src, dst)
	}
	put(t, s, "retry/k", "x")
	if calls != 3 { // data retried once, then the sidecar
		t.Errorf("rename calls = %d", calls)
	}
	rename = func(*os.Root, string, string) error { return errors.New("disk full") }
	if _, err := s.PutObject("bkt", "fail", bytes.NewReader([]byte("x")), 1, PutOptions{}); err == nil {
		t.Error("rename failure ignored")
	}
}

func TestBucketTags(t *testing.T) {
	s := newStore(t)
	if tags, err := s.BucketTags("bkt"); err != nil || tags != nil {
		t.Errorf("untagged: %v %v", tags, err)
	}
	// A bucket made outside the server keeps its directory's time as its creation time.
	outside := filepath.Join(s.root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	_ = os.Chtimes(outside, old, old)
	if err := s.SetBucketTags("outside", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.sys("buckets", "outside.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removing no tags wrote a record: %v", err)
	}
	if err := s.SetBucketTags("outside", []Tag{{Key: "a", Value: "1"}}); err != nil {
		t.Fatal(err)
	}
	buckets, _ := s.ListBuckets()
	for _, b := range buckets {
		if b.Name == "outside" && !b.Created.Equal(old) {
			t.Errorf("created %v, want %v", b.Created, old)
		}
	}
	if tags, err := s.BucketTags("outside"); err != nil || len(tags) != 1 || tags[0].Key != "a" {
		t.Errorf("tags: %v %v", tags, err)
	}
	if _, err := s.BucketTags("nope"); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
	if err := s.SetBucketTags("nope", nil); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("tagging a missing bucket: %v", err)
	}
}

// A put syncs each directory it added an entry to, after the rename, and not the ones it
// left unchanged.
func TestPutSyncsDirectories(t *testing.T) {
	s := newStore(t)
	var synced []string
	old := syncDir
	syncDir = func(r *os.Root, dir string) {
		if dir == "bkt/a" {
			if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "a", "b.txt")); err != nil {
				t.Errorf("bkt/a synced before the object was in it: %v", err)
			}
		}
		synced = append(synced, filepath.ToSlash(dir))
		old(r, dir)
	}
	t.Cleanup(func() { syncDir = old })
	put(t, s, "a/b.txt", "x")
	meta, _ := filepath.Rel(s.Root(), filepath.Dir(s.metaPath("bkt", "a/b.txt")))
	// bkt/a was created, so bkt gained an entry too.
	for _, want := range []string{"bkt/a", "bkt", filepath.ToSlash(meta)} {
		if !slices.Contains(synced, want) {
			t.Errorf("synced %v, missing %s", synced, want)
		}
	}
	synced = nil
	put(t, s, "a/c.txt", "x")
	if slices.Contains(synced, "bkt") {
		t.Errorf("synced %v: bkt did not change", synced)
	}
	synced = nil
	if _, err := s.PutObject("bkt", "m/n/", strings.NewReader(""), 0, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, "bkt/m") || !slices.Contains(synced, "bkt") {
		t.Errorf("a new marker synced %v, want bkt and bkt/m", synced)
	}
	synced = nil
	if err := s.CreateBucket("other"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, ".") {
		t.Errorf("a new bucket synced %v, want the root", synced)
	}
}
