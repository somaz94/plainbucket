package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultipartLifecycle(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	id, err := s.CreateUpload("bkt", "big/file.bin", Metadata{ContentType: "application/x-test"})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := s.PutPart("bkt", "big/file.bin", id, 1, strings.NewReader("aaaa"), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Re-uploading a part replaces it.
	if _, err := s.PutPart("bkt", "big/file.bin", id, 2, strings.NewReader("zzzz"), 4, nil); err != nil {
		t.Fatal(err)
	}
	p2, err := s.PutPart("bkt", "big/file.bin", id, 2, strings.NewReader("bbbb"), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := s.PutPart("bkt", "big/file.bin", id, 3, strings.NewReader("c"), 1, func([]byte) (map[string]string, error) {
		return map[string]string{"CRC32": "v"}, nil
	})
	if err != nil || p3.Checksums["CRC32"] != "v" {
		t.Fatalf("part 3: %+v %v", p3, err)
	}

	parts, truncated, err := s.ListParts("bkt", "big/file.bin", id, 0, 2)
	if err != nil || !truncated || len(parts) != 2 || parts[1].Number != 2 || parts[1].ETag != p2.ETag {
		t.Fatalf("list parts: %+v %v %v", parts, truncated, err)
	}
	parts, truncated, _ = s.ListParts("bkt", "big/file.bin", id, 2, 10)
	if truncated || len(parts) != 1 || parts[0].Number != 3 {
		t.Errorf("list parts after marker: %+v", parts)
	}

	ups, _, err := s.ListUploads("bkt", "big/", "", "", 10)
	if err != nil || len(ups) != 1 || ups[0].UploadID != id {
		t.Errorf("list uploads: %+v %v", ups, err)
	}

	info, err := s.CompleteUpload(context.Background(), "bkt", "big/file.bin", id, []CompletePart{
		{1, p1.ETag}, {2, strings.Trim(p2.ETag, `"`)}, {3, p3.ETag},
	}, Conditions{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := md5.New()
	for _, e := range []string{p1.ETag, p2.ETag, p3.ETag} {
		raw, _ := hex.DecodeString(strings.Trim(e, `"`))
		h.Write(raw)
	}
	if want := `"` + hex.EncodeToString(h.Sum(nil)) + `-3"`; info.ETag != want || info.Size != 9 {
		t.Errorf("complete = %+v, want ETag %s", info, want)
	}
	body, got := read(t, s, "big/file.bin")
	if body != "aaaabbbbc" || got.ContentType != "application/x-test" || got.ETag != info.ETag {
		t.Errorf("assembled %q %+v", body, got)
	}
	if _, err := os.Stat(s.sys("uploads", id)); !os.IsNotExist(err) {
		t.Error("upload directory kept after completion")
	}
	if _, err := s.CompleteUpload(context.Background(), "bkt", "big/file.bin", id, []CompletePart{{1, p1.ETag}}, Conditions{}, 0); !errors.Is(err, ErrNoSuchUpload) {
		t.Errorf("complete twice: %v", err)
	}
}

func TestMultipartErrors(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	id, _ := s.CreateUpload("bkt", "k", Metadata{})
	p1, _ := s.PutPart("bkt", "k", id, 1, strings.NewReader("abc"), 3, nil)
	p2, _ := s.PutPart("bkt", "k", id, 2, strings.NewReader("abcd"), 4, nil)

	cases := []struct {
		name  string
		parts []CompletePart
		want  error
	}{
		{"empty", nil, ErrInvalidPart},
		{"order", []CompletePart{{2, p2.ETag}, {1, p1.ETag}}, ErrInvalidPartOrder},
		{"duplicate", []CompletePart{{1, p1.ETag}, {1, p1.ETag}}, ErrInvalidPartOrder},
		{"etag", []CompletePart{{1, `"0000"`}}, ErrInvalidPart},
		{"missing", []CompletePart{{7, p1.ETag}}, ErrInvalidPart},
		{"too small", []CompletePart{{1, p1.ETag}, {2, p2.ETag}}, ErrEntityTooSmall},
	}
	for _, tc := range cases {
		if _, err := s.CompleteUpload(context.Background(), "bkt", "k", id, tc.parts, Conditions{}, 0); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	for _, n := range []int{0, 10001} {
		if _, err := s.PutPart("bkt", "k", id, n, strings.NewReader(""), 0, nil); !errors.Is(err, ErrInvalidPartNumber) {
			t.Errorf("part %d: %v", n, err)
		}
	}
	for _, bad := range []string{"../../etc", strings.Repeat("0", 32)} {
		if _, err := s.PutPart("bkt", "k", bad, 1, strings.NewReader(""), 0, nil); !errors.Is(err, ErrNoSuchUpload) {
			t.Errorf("upload id %q: %v", bad, err)
		}
	}
	if _, err := s.PutPart("bkt", "other-key", id, 1, strings.NewReader(""), 0, nil); !errors.Is(err, ErrNoSuchUpload) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := s.PutPart("bkt", "k", id, 1, strings.NewReader("ab"), 3, nil); !errors.Is(err, ErrShortBody) {
		t.Errorf("short part: %v", err)
	}
	if _, err := s.CreateUpload("bkt", "dir/", Metadata{}); !errors.Is(err, ErrMarkerNotEmpty) {
		t.Errorf("upload to a marker key: %v", err)
	}
	if _, err := s.CreateUpload("nope", "k", Metadata{}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}

	// The last part may be small; the condition is checked on completion.
	put(t, s, "k", "existing")
	_, err := s.CompleteUpload(context.Background(), "bkt", "k", id, []CompletePart{{2, p2.ETag}}, Conditions{IfNoneMatch: "*"}, 0)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("conditional complete: %v", err)
	}
	if err := s.AbortUpload("bkt", "k", id); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUpload("bkt", "k", id); !errors.Is(err, ErrNoSuchUpload) {
		t.Errorf("abort twice: %v", err)
	}
	if _, _, err := s.ListParts("bkt", "k", id, 0, 10); !errors.Is(err, ErrNoSuchUpload) {
		t.Errorf("list parts after abort: %v", err)
	}
}

func TestListUploadsOrderAndMarkers(t *testing.T) {
	s := newStore(t)
	_ = s.CreateBucket("other")
	a1, _ := s.CreateUpload("bkt", "a", Metadata{})
	a2, _ := s.CreateUpload("bkt", "a", Metadata{})
	b1, _ := s.CreateUpload("bkt", "b", Metadata{})
	_, _ = s.CreateUpload("other", "a", Metadata{})
	if a2 < a1 {
		a1, a2 = a2, a1
	}
	ups, truncated, _ := s.ListUploads("bkt", "", "", "", 2)
	if !truncated || len(ups) != 2 || ups[0].UploadID != a1 || ups[1].UploadID != a2 {
		t.Fatalf("page 1: %+v", ups)
	}
	ups, truncated, _ = s.ListUploads("bkt", "", "a", a2, 2)
	if truncated || len(ups) != 1 || ups[0].UploadID != b1 {
		t.Errorf("after marker: %+v", ups)
	}
	ups, _, _ = s.ListUploads("bkt", "", "a", "", 10)
	if len(ups) != 1 || ups[0].Key != "b" {
		t.Errorf("key marker only: %+v", ups)
	}
	if _, _, err := s.ListUploads("nope", "", "", "", 1); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestSweep(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }

	put(t, s, "kept", "x")
	put(t, s, "orphan", "x")
	_ = os.Remove(filepath.Join(s.Root(), "bkt", "orphan")) // deleted outside the server
	_ = os.WriteFile(s.sys("meta", "bkt", "zz", "garbage.json"), []byte("{"), 0o600)
	_ = os.MkdirAll(s.sys("meta", "bkt", "zz"), 0o750)
	_ = os.WriteFile(s.sys("meta", "bkt", "zz", "garbage.json"), []byte("{"), 0o600)
	_ = os.MkdirAll(s.sys("meta", "gone-bucket"), 0o750)
	_ = os.WriteFile(s.sys("buckets", "gone-bucket.json"), []byte("{}"), 0o600)

	old := now.Add(-48 * time.Hour)
	stale := s.sys("tmp", "upload-stale")
	_ = os.WriteFile(stale, nil, 0o600)
	_ = os.Chtimes(stale, old, old)
	fresh := s.sys("tmp", "upload-fresh")
	_ = os.WriteFile(fresh, nil, 0o600)

	expired, _ := s.CreateUpload("bkt", "e", Metadata{})
	live, _ := s.CreateUpload("bkt", "l", Metadata{})
	raw, _ := os.ReadFile(filepath.Join(s.sys("uploads", expired), uploadMetaFile))
	_ = os.WriteFile(filepath.Join(s.sys("uploads", expired), uploadMetaFile),
		[]byte(strings.Replace(string(raw), now.UTC().Format("2006"), "2001", 1)), 0o600)
	headless := s.sys("uploads", strings.Repeat("a", 32))
	_ = os.MkdirAll(headless, 0o750)
	_ = os.Chtimes(headless, old, old)

	n := s.Sweep(context.Background(), 7*24*time.Hour)
	if n != 7 {
		t.Errorf("removed %d entries, want 7", n)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	checks := map[string]bool{
		s.metaPath("bkt", "kept"):            true,
		s.metaPath("bkt", "orphan"):          false,
		s.sys("meta", "gone-bucket"):         false,
		s.sys("buckets", "gone-bucket.json"): false,
		s.sys("buckets", "bkt.json"):         true,
		stale:                                false,
		fresh:                                true,
		s.sys("uploads", expired):            false,
		s.sys("uploads", live):               true,
		headless:                             false,
	}
	for p, want := range checks {
		if exists(p) != want {
			t.Errorf("%s exists = %v, want %v", p, !want, want)
		}
	}
}

func TestRunJanitorStops(t *testing.T) {
	s := newStore(t)
	stale := s.sys("tmp", "upload-old")
	_ = os.WriteFile(stale, nil, 0o600)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunJanitor(ctx, 10*time.Millisecond, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("janitor never swept")
	}
}

func TestMeasureUsage(t *testing.T) {
	s := newStore(t)
	if !s.Usage().At.IsZero() {
		t.Error("usage before measuring")
	}
	put(t, s, "a/b", "12345")
	put(t, s, "c", "123")
	put(t, s, "dir/", "")
	_ = s.CreateBucket("other")
	_, _ = s.PutObject("other", "x", strings.NewReader("12"), 2, PutOptions{})
	s.MeasureUsage(context.Background())
	if u := s.Usage(); u.Objects != 3 || u.Bytes != 10 || u.At.IsZero() {
		t.Errorf("usage = %+v", u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := s.Usage()
	put(t, s, "d", "1")
	s.MeasureUsage(ctx)
	if s.Usage() != before {
		t.Error("a cancelled measurement replaced the last one")
	}
}

func TestPutPartCopy(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	src := put(t, s, "src", "0123456789")
	id, err := s.CreateUpload("bkt", "dst", Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p1, err := s.PutPartCopy(ctx, "bkt", "src", &ByteRange{Start: 0, End: 5}, "bkt", "dst", id, 1, CopyOptions{SrcIfMatch: src.ETag})
	if err != nil || p1.Size != 6 || p1.ETag != md5ETag(md5sum("012345")) {
		t.Fatalf("part 1: %+v %v", p1, err)
	}
	p2, err := s.PutPartCopy(ctx, "bkt", "src", &ByteRange{Start: 6, End: 9}, "bkt", "dst", id, 2, CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.CompleteUpload(ctx, "bkt", "dst", id, []CompletePart{{Number: 1, ETag: p1.ETag}, {Number: 2, ETag: p2.ETag}}, Conditions{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.root, "bkt", "dst")); string(got) != "0123456789" || info.Size != 10 {
		t.Errorf("assembled %q (%d)", got, info.Size)
	}

	whole, _ := s.CreateUpload("bkt", "whole", Metadata{})
	if p, err := s.PutPartCopy(ctx, "bkt", "src", nil, "bkt", "whole", whole, 1, CopyOptions{}); err != nil || p.Size != 10 {
		t.Errorf("whole object part: %+v %v", p, err)
	}

	id, _ = s.CreateUpload("bkt", "dst2", Metadata{})
	for name, c := range map[string]struct {
		rng  *ByteRange
		o    CopyOptions
		src  string
		n    int
		id   string
		want error
	}{
		"past end":       {rng: &ByteRange{Start: 5, End: 10}, src: "src", n: 1, id: id, want: ErrInvalidRange},
		"reversed":       {rng: &ByteRange{Start: 5, End: 4}, src: "src", n: 1, id: id, want: ErrInvalidRange},
		"if-match":       {o: CopyOptions{SrcIfMatch: `"nope"`}, src: "src", n: 1, id: id, want: ErrPreconditionFailed},
		"missing":        {src: "missing", n: 1, id: id, want: ErrNoSuchKey},
		"bad number":     {src: "src", n: 0, id: id, want: ErrInvalidPartNumber},
		"unknown upload": {src: "src", n: 1, id: strings.Repeat("0", 32), want: ErrNoSuchUpload},
	} {
		if _, err := s.PutPartCopy(ctx, "bkt", c.src, c.rng, "bkt", "dst2", c.id, c.n, c.o); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	marker, _ := s.CreateUpload("bkt", "m", Metadata{})
	if _, err := s.PutObject("bkt", "dir/", strings.NewReader(""), 0, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPartCopy(ctx, "bkt", "dir/", nil, "bkt", "m", marker, 1, CopyOptions{}); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("marker source: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PutPartCopy(canceled, "bkt", "src", nil, "bkt", "m", marker, 1, CopyOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}
