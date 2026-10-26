package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/checksum"
)

// rot rewrites the data of key in place, keeping its size and mtime, as a disk that
// flips bits would: the metadata still looks valid.
func rot(t *testing.T, s *Store, key string, at int64) {
	t.Helper()
	p := filepath.Join(s.root, "bkt", key)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{'#'}, at); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func multipart(t *testing.T, s *Store, key string, parts ...string) {
	t.Helper()
	id, err := s.CreateUpload("bkt", key, Metadata{}, MultipartChecksum{})
	if err != nil {
		t.Fatal(err)
	}
	var done []CompletePart
	for i, p := range parts {
		info, err := s.PutPart("bkt", key, id, i+1, strings.NewReader(p), int64(len(p)), nil)
		if err != nil {
			t.Fatal(err)
		}
		done = append(done, CompletePart{i + 1, info.ETag})
	}
	if _, err := s.CompleteUpload(context.Background(), "bkt", key, id, done, Conditions{}, 0, CompleteChecksums{}); err != nil {
		t.Fatal(err)
	}
}

func scrub(t *testing.T, s *Store) (ScrubResult, []Corruption) {
	t.Helper()
	var got []Corruption
	res, err := s.Scrub(context.Background(), 0, func(c Corruption) { got = append(got, c) })
	if err != nil {
		t.Fatal(err)
	}
	return res, got
}

func TestScrub(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	if _, ok := s.LastScrub(); ok {
		t.Fatal("LastScrub before any scrub")
	}
	put(t, s, "single", "hello world")
	multipart(t, s, "multi", "aaaa", "bbbb", "cc")
	put(t, s, "dir/", "")
	if _, err := s.CopyObject(context.Background(), "bkt", "multi", "bkt", "copy", CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "edited", "x")

	res, got := scrub(t, s)
	if len(got) != 0 || res.Objects != 4 || res.Bytes != 11+10+10+1 || res.Corrupt != 0 {
		t.Fatalf("intact: %+v %+v", res, got)
	}
	if last, ok := s.LastScrub(); !ok || last.Objects != 4 || last.Finished.IsZero() {
		t.Errorf("LastScrub: %+v %v", last, ok)
	}

	rot(t, s, "single", 0)
	rot(t, s, "multi", 5) // in the second part
	// An outside edit moves the mtime: the metadata is no longer trusted, so it is skipped.
	if err := os.WriteFile(filepath.Join(s.root, "bkt", "edited"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(s.root, "bkt", "edited"), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	res, got = scrub(t, s)
	if res.Corrupt != 2 || res.Skipped != 1 || len(got) != 2 {
		t.Fatalf("rotten: %+v %+v", res, got)
	}
	for _, c := range got {
		if c.What != "etag" || c.Want == c.Got || (c.Key != "single" && c.Key != "multi") {
			t.Errorf("finding: %+v", c)
		}
		if c.Key == "multi" && !strings.HasSuffix(c.Got, `-3"`) {
			t.Errorf("multipart ETag not recomputed from its parts: %+v", c)
		}
	}
}

func TestScrubChecksum(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "data")
	record := func(v string) {
		t.Helper()
		m, _ := s.readSidecar("bkt", "k")
		m.Checksums = map[string]string{"CRC32": v}
		raw, _ := json.Marshal(m)
		if err := s.writeFileAtomic(s.metaPath("bkt", "k"), raw); err != nil {
			t.Fatal(err)
		}
	}
	sum, _ := checksum.New("CRC32")
	sum.Write([]byte("data"))
	record(checksum.Encode(sum.Sum(nil)))
	if _, got := scrub(t, s); len(got) != 0 {
		t.Fatalf("matching checksum reported: %+v", got)
	}
	record("AAAAAA==")
	if _, got := scrub(t, s); len(got) != 1 || got[0].What != "CRC32" || got[0].Want != "AAAAAA==" {
		t.Errorf("checksum finding: %+v", got)
	}
}

func TestScrubRateAndCancel(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", strings.Repeat("x", 2000))
	start := time.Now()
	if _, err := s.Scrub(context.Background(), 10000, func(Corruption) {}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("2000 bytes at 10000/s took %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Scrub(ctx, 1, func(Corruption) {}); err == nil {
		t.Error("canceled scrub returned no error")
	}
}

func TestScrubEdgeCases(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	put(t, s, "empty", "")
	multipart(t, s, "tail", "aaaa", "")
	put(t, s, "legacy", "x")
	multipart(t, s, "legacy-multi", "aaaa", "bb")
	put(t, s, "composite", "y")
	edit := func(key string, fn func(*sidecar)) {
		t.Helper()
		m, _ := s.readSidecar("bkt", key)
		fn(&m)
		raw, _ := json.Marshal(m)
		if err := s.writeFileAtomic(s.metaPath("bkt", key), raw); err != nil {
			t.Fatal(err)
		}
	}
	// A multipart upload completed by v0.1.x recorded no part sizes.
	edit("legacy-multi", func(m *sidecar) { m.Parts = nil })
	// Composite checksums are not re-read, so a wrong one is not a finding.
	edit("composite", func(m *sidecar) {
		m.ChecksumType, m.Checksums = ChecksumComposite, map[string]string{"CRC32": "AAAAAA==-2"}
	})
	// A symlink in a bucket is not an object, wherever it points.
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("s"), 0o600)
	if err := os.Symlink(outside, filepath.Join(s.root, "bkt", "link")); err != nil {
		t.Fatal(err)
	}
	// A restored object keeps data and metadata together.
	s.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	put(t, s, "restored", "back")
	if err := s.DeleteObject("bkt", "restored"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListTrash("bkt")
	if _, err := s.RestoreTrash("bkt", list[0].ID); err != nil {
		t.Fatal(err)
	}

	res, got := scrub(t, s)
	if len(got) != 0 || res.Unchecked != 1 || res.Objects != 5 || res.Bytes != 10 {
		t.Errorf("edge cases: %+v %+v", res, got)
	}

	// The result outlives the process.
	again, err := New(s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if last, ok := again.LastScrub(); !ok || last.Objects != res.Objects || !last.Finished.Equal(res.Finished) {
		t.Errorf("reloaded LastScrub: %+v %v", last, ok)
	}
}

func TestScrubReadFailures(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "data")
	orig := scrubRead
	t.Cleanup(func() { scrubRead = orig })

	// An unreadable sector is damage, not a skip.
	scrubRead = func(*os.File, []byte) (int, error) { return 0, syscall.EIO }
	res, got := scrub(t, s)
	if res.Corrupt != 1 || len(got) != 1 || got[0].What != "read" {
		t.Errorf("read error: %+v %+v", res, got)
	}

	// An object rewritten while it is read is skipped, whatever it hashed to.
	scrubRead = func(f *os.File, b []byte) (int, error) {
		scrubRead = orig
		put(t, s, "k", "DATA")
		return orig(f, b)
	}
	res, got = scrub(t, s)
	if res.Skipped != 1 || len(got) != 0 {
		t.Errorf("rewritten during the read: %+v %+v", res, got)
	}
}

func TestScrubCancelWhilePacing(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", strings.Repeat("x", 2000))
	first, _ := scrub(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Scrub(ctx, 1000, func(Corruption) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("canceled while pacing: %v", err)
	}
	if last, _ := s.LastScrub(); !last.Finished.Equal(first.Finished) {
		t.Error("a canceled scrub replaced the last result")
	}
}

func TestPacerEarnsNoBurst(t *testing.T) {
	p := pacer{rate: 10000}
	ctx := context.Background()
	_ = p.wait(ctx, 100)
	time.Sleep(200 * time.Millisecond) // idle, as while walking a large tree
	start := time.Now()
	if err := p.wait(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("1000 bytes at 10000/s after idling took %v", d)
	}
	if (&pacer{}).wait(ctx, 1<<30) != nil {
		t.Error("an unlimited pacer waited")
	}
}
