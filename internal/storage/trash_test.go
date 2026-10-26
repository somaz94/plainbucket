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

func TestTrash(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	s.now = c.now
	s.SetTrash(map[string]time.Duration{"bkt": 24 * time.Hour})
	if s.TrashAfter("bkt") != 24*time.Hour || s.TrashAfter("other") != 0 {
		t.Fatal("TrashAfter")
	}
	ctx := context.Background()
	s.MeasureUsage(ctx)
	tags := []Tag{{"a", "1"}}
	if _, err := s.PutObject("bkt", "dir/k", strings.NewReader("hello"), 5, PutOptions{Metadata: Metadata{ContentType: "text/x", Tags: tags}}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "dir/other", "x")
	if removed, err := s.DeleteObjectIf("bkt", "dir/k", DeleteConditions{}); err != nil || !removed {
		t.Fatal(removed, err)
	}
	if exists(s, "dir/k") {
		t.Fatal("still in the bucket")
	}
	if u := s.AllBucketUsage()["bkt"]; u.Objects != 1 || u.Bytes != 1 {
		t.Errorf("usage after trashing: %+v", u)
	}
	list, err := s.ListTrash("bkt")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTrash: %+v %v", list, err)
	}
	e := list[0]
	if e.Key != "dir/k" || e.Size != 5 || !e.DeletedAt.Equal(c.t) || !e.ExpiresAt.Equal(c.t.Add(24*time.Hour)) {
		t.Errorf("entry: %+v", e)
	}
	if other, _ := s.ListTrash("nope"); len(other) != 0 {
		t.Errorf("another bucket's trash: %v", other)
	}

	// Restoring puts back the data and the metadata, and never replaces an object.
	put(t, s, "dir/k", "newer")
	if _, err := s.RestoreTrash("bkt", e.ID); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("restore over an object: %v", err)
	}
	if err := s.DeleteObject("bkt", "dir/k"); err != nil {
		t.Fatal(err)
	}
	info, err := s.RestoreTrash("bkt", e.ID)
	if err != nil || info.ContentType != "text/x" || !reflect.DeepEqual(info.Tags, tags) {
		t.Fatalf("restore: %+v %v", info, err)
	}
	if got, info := read(t, s, "dir/k"); got != "hello" || info.ContentType != "text/x" {
		t.Errorf("restored: %q %+v", got, info)
	}
	if u := s.AllBucketUsage()["bkt"]; u.Objects != 2 || u.Bytes != 6 {
		t.Errorf("usage after restoring: %+v", u)
	}
	if _, err := s.RestoreTrash("bkt", e.ID); !errors.Is(err, ErrNoSuchTrash) {
		t.Errorf("restored twice: %v", err)
	}

	// The second delete of dir/k ("newer") is still in the trash; purge removes it.
	list, _ = s.ListTrash("bkt")
	if len(list) != 1 || list[0].Size != 5 {
		t.Fatalf("after restore: %+v", list)
	}
	for _, bad := range []struct{ bucket, id string }{{"bkt", "../meta"}, {"other", list[0].ID}} {
		if err := s.PurgeTrash(bad.bucket, bad.id); !errors.Is(err, ErrNoSuchTrash) {
			t.Errorf("purge %+v: %v", bad, err)
		}
	}
	if err := s.PurgeTrash("bkt", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListTrash("bkt"); len(list) != 0 {
		t.Errorf("after purge: %+v", list)
	}

	// Entries expire, then the sweep removes them; markers and other buckets delete at once.
	if err := s.DeleteObject("bkt", "dir/other"); err != nil {
		t.Fatal(err)
	}
	put(t, s, "m/", "")
	if err := s.DeleteObject("bkt", "m/"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(25 * time.Hour)
	if list, _ = s.ListTrash("bkt"); len(list) != 0 {
		t.Errorf("expired entries listed: %+v", list)
	}
	if n := s.Sweep(ctx, time.Hour); n != 1 {
		t.Errorf("swept %d", n)
	}
	if ents, _ := os.ReadDir(s.trashDir("bkt")); len(ents) != 0 {
		t.Errorf("trash left: %d", len(ents))
	}
	if err := s.CreateBucket("plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("plain", "k", strings.NewReader("x"), 1, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject("plain", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.trashDir("plain")); !os.IsNotExist(err) {
		t.Errorf("a bucket without trash-after trashed: %v", err)
	}
}

func TestTrashRestoreConflicts(t *testing.T) {
	s := newStore(t)
	s.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	put(t, s, "a", "x")
	if err := s.DeleteObject("bkt", "a"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListTrash("bkt")
	put(t, s, "a/b", "y")
	if _, err := s.RestoreTrash("bkt", list[0].ID); !errors.Is(err, ErrConflict) {
		t.Errorf("restore onto a directory: %v", err)
	}

	// A refusing limit applies to restores as to writes.
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 1}})
	s.MeasureUsage(context.Background())
	if err := s.DeleteObject("bkt", "a/b"); err != nil {
		t.Fatal(err)
	}
	put(t, s, "c", "z")
	list, _ = s.ListTrash("bkt")
	for _, e := range list {
		if e.Key == "a/b" {
			if _, err := s.RestoreTrash("bkt", e.ID); !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("restore past max-size: %v", err)
			}
		}
	}

	// A manifest without its data (an interrupted delete) is not listed and is swept later.
	dir := s.trashDir("bkt", "0123456789abcdef0123456789abcdef")
	_ = os.Mkdir(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, trashManifest), []byte(`{"id":"0123456789abcdef0123456789abcdef","bucket":"bkt","key":"x","expires_at":"2999-01-01T00:00:00Z"}`), 0o600)
	if list, _ = s.ListTrash("bkt"); len(list) != 2 {
		t.Errorf("listed an entry without data: %+v", list)
	}
	if _, err := s.RestoreTrash("bkt", "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrNoSuchTrash) {
		t.Errorf("restore without data: %v", err)
	}
	junk := s.trashDir("bkt", "junk")
	_ = os.Mkdir(junk, 0o750)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(junk, old, old)
	if n := s.Sweep(context.Background(), time.Hour); n != 1 {
		t.Errorf("swept %d, want the unreadable entry", n)
	}
}

func TestTrashEdgeCases(t *testing.T) {
	s := newStore(t)
	s.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	ctx := context.Background()

	// A file written outside the server has no trusted sidecar to keep; the restore
	// still reads it as just used, so expiry does not take it at once.
	old := time.Now().Add(-72 * time.Hour)
	p := filepath.Join(s.root, "bkt", "outside")
	_ = os.WriteFile(p, []byte("raw"), 0o644)
	_ = os.Chtimes(p, old, old)
	if err := s.DeleteObject("bkt", "outside"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListTrash("bkt")
	s.SetLimits(map[string]Limit{"bkt": {ExpireAfter: 24 * time.Hour}})
	info, err := s.RestoreTrash("bkt", list[0].ID)
	if err != nil || info.ETag == "" || info.Size != 3 {
		t.Fatalf("restore without a sidecar: %+v %v", info, err)
	}
	if expired, _ := s.enforceLimits(ctx); expired != 0 || !exists(s, "outside") {
		t.Error("a restored object expired at once")
	}
	s.SetLimits(nil)

	// A restore under a key whose parent is now a file is a conflict.
	put(t, s, "p/q", "1")
	if err := s.DeleteObject("bkt", "p/q"); err != nil {
		t.Fatal(err)
	}
	put(t, s, "p", "file")
	list, _ = s.ListTrash("bkt")
	if _, err := s.RestoreTrash("bkt", list[0].ID); !errors.Is(err, ErrConflict) {
		t.Errorf("restore under a file: %v", err)
	}

	// A manifest whose key leaves the bucket is never restored.
	dir := s.trashDir("bkt", "fedcba9876543210fedcba9876543210")
	_ = os.Mkdir(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, trashData), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, trashManifest), []byte(`{"id":"fedcba9876543210fedcba9876543210","bucket":"bkt","key":"../other/x","expires_at":"2999-01-01T00:00:00Z"}`), 0o600)
	if _, err := s.RestoreTrash("bkt", "fedcba9876543210fedcba9876543210"); !errors.Is(err, ErrNoSuchTrash) {
		t.Errorf("escaping key: %v", err)
	}
	for _, bucket := range []string{"../meta", "Bad"} {
		if l, err := s.ListTrash(bucket); err != nil || len(l) != 0 {
			t.Errorf("ListTrash(%q) = %v %v", bucket, l, err)
		}
		if _, err := s.RestoreTrash(bucket, "fedcba9876543210fedcba9876543210"); !errors.Is(err, ErrNoSuchTrash) {
			t.Errorf("RestoreTrash(%q): %v", bucket, err)
		}
	}

	// A move into the trash that fails keeps the object and leaves no entry.
	put(t, s, "stays", "1")
	orig := rename
	t.Cleanup(func() { rename = orig })
	rename = func(*os.Root, string, string) error { return errors.New("injected") }
	if err := s.DeleteObject("bkt", "stays"); err == nil {
		t.Error("delete with a failing move succeeded")
	}
	rename = orig
	if !exists(s, "stays") {
		t.Error("object lost by a failed move")
	}
	before := len(list)
	if list, _ = s.ListTrash("bkt"); len(list) != before {
		t.Errorf("entries after a failed move: %d, want %d", len(list), before)
	}
}
