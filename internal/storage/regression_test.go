package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A GET that hashed a file must not overwrite the sidecar a concurrent PUT landed.
func TestRefreshKeepsFreshSidecar(t *testing.T) {
	s := newStore(t)
	_, err := s.PutObject("bkt", "k", strings.NewReader("data"), 4, PutOptions{Metadata: Metadata{ContentType: "text/x-upload"}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.r.Open(filepath.Join("bkt", "k"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	m, err := s.refresh("bkt", "k", f, fi, sidecar{}, true)
	if err != nil || m.ContentType != "text/x-upload" {
		t.Errorf("refresh returned %+v, %v", m, err)
	}
	if got, _ := s.readSidecar("bkt", "k"); got.ContentType != "text/x-upload" {
		t.Errorf("sidecar overwritten: %+v", got)
	}
}

// A GET holding the replaced file must not describe the new file with the old bytes.
func TestRefreshSkipsReplacedFile(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "old")
	f, err := s.r.Open(filepath.Join("bkt", "k"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	_ = os.Remove(s.metaPath("bkt", "k"))
	fresh, _ := s.PutObject("bkt", "k", strings.NewReader("new!"), 4, PutOptions{Metadata: Metadata{ContentType: "text/new"}})
	if _, err := s.refresh("bkt", "k", f, fi, sidecar{}, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.readSidecar("bkt", "k"); got.ETag != fresh.ETag || got.ContentType != "text/new" {
		t.Errorf("sidecar = %+v, want the new upload's", got)
	}
}

func TestCompleteTooLargeKeepsObject(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 1
	put(t, s, "k", "previous")
	id, _ := s.CreateUpload("bkt", "k", Metadata{}, MultipartChecksum{})
	p1, _ := s.PutPart("bkt", "k", id, 1, strings.NewReader("12345"), 5, nil)
	p2, _ := s.PutPart("bkt", "k", id, 2, strings.NewReader("12345"), 5, nil)
	_, err := s.CompleteUpload(context.Background(), "bkt", "k", id, []CompletePart{{1, p1.ETag}, {2, p2.ETag}}, Conditions{}, 8, CompleteChecksums{})
	if !errors.Is(err, ErrEntityTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if body, _ := read(t, s, "k"); body != "previous" {
		t.Errorf("object = %q", body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CompleteUpload(ctx, "bkt", "k", id, []CompletePart{{1, p1.ETag}, {2, p2.ETag}}, Conditions{}, 0, CompleteChecksums{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled complete: %v", err)
	}
	if body, _ := read(t, s, "k"); body != "previous" {
		t.Errorf("object after cancelled complete = %q", body)
	}
}

// A commit whose data rename fails leaves the previous object and its metadata intact.
func TestFailedCommitKeepsMetadata(t *testing.T) {
	s := newStore(t)
	if _, err := s.PutObject("bkt", "k", strings.NewReader("v1"), 2, PutOptions{Metadata: Metadata{ContentType: "text/v1"}}); err != nil {
		t.Fatal(err)
	}
	orig := rename
	t.Cleanup(func() { rename = orig })
	rename = func(r *os.Root, src, dst string) error {
		if dst == filepath.Join("bkt", "k") {
			return errors.New("EXDEV")
		}
		return orig(r, src, dst)
	}
	if _, err := s.PutObject("bkt", "k", strings.NewReader("v2"), 2, PutOptions{}); err == nil {
		t.Fatal("failed rename reported success")
	}
	rename = orig
	if body, info := read(t, s, "k"); body != "v1" || info.ContentType != "text/v1" {
		t.Errorf("after failed commit: %q %+v", body, info)
	}

	// A sidecar that fails to land must not leave the old one describing the new data.
	rename = func(r *os.Root, src, dst string) error {
		if strings.Contains(dst, filepath.Join(SysDir, "meta")) {
			return errors.New("disk full")
		}
		return orig(r, src, dst)
	}
	if _, err := s.PutObject("bkt", "k", strings.NewReader("v3"), 2, PutOptions{}); err == nil {
		t.Fatal("failed sidecar reported success")
	}
	rename = orig
	if _, err := os.Stat(s.metaPath("bkt", "k")); !os.IsNotExist(err) {
		t.Error("old sidecar kept after the new one failed")
	}
	if body, info := read(t, s, "k"); body != "v3" || info.ContentType == "text/v1" {
		t.Errorf("after failed sidecar: %q %+v", body, info)
	}
}

func TestExplicitMarkerSurvivesPrune(t *testing.T) {
	s := newStore(t)
	put(t, s, "a/", "")
	put(t, s, "a/b/f", "x")
	put(t, s, "m/x", "y")
	if got := listAll(t, s, ListParams{}, 100); !reflect.DeepEqual(got, []string{"a/", "a/b/f", "m/x"}) {
		t.Errorf("listing = %q", got)
	}
	if err := s.DeleteObject("bkt", "a/b/f"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "a", "b")); !os.IsNotExist(err) {
		t.Error("implicit directory below the marker kept")
	}
	if _, info, err := s.GetObject("bkt", "a/"); err != nil || !info.IsDir {
		t.Errorf("explicit marker removed: %v", err)
	}
	if err := s.DeleteObject("bkt", "a/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "bkt", "a")); !os.IsNotExist(err) {
		t.Error("marker directory kept after deleting the marker")
	}
	if n := s.Sweep(context.Background(), 0); n != 0 {
		t.Errorf("sweep removed %d live entries", n)
	}
}

func TestDeleteBucketWaitsForWriters(t *testing.T) {
	s := newStore(t)
	bl := s.bucketLock("bkt")
	bl.RLock() // a commit in flight
	done := make(chan error, 1)
	go func() { done <- s.DeleteBucket("bkt") }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("DeleteBucket did not wait for the writer")
	default:
	}
	bl.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
