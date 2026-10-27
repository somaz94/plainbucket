package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventory(t *testing.T) {
	s := newStore(t)
	sums := func([]byte) (map[string]string, error) {
		return map[string]string{"SHA256": "c2hh", "CRC32": "Y3Jj"}, nil
	}
	if _, err := s.PutObject("bkt", "a/rec", strings.NewReader("abc"), 3, PutOptions{Metadata: Metadata{ContentType: "text/plain"}, Verify: sums}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "a/edit", "12")
	put(t, s, "dir/", "")
	_ = os.WriteFile(filepath.Join(s.root, "bkt", "a", "edit"), []byte("123"), 0o644)
	_ = os.WriteFile(filepath.Join(s.root, "bkt", "b-dropped"), []byte("x"), 0o644)
	if err := s.CreateBucket("other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("other", "a/z", strings.NewReader("z"), 1, PutOptions{}); err != nil {
		t.Fatal(err)
	}

	collect := func(bucket, prefix string) []InventoryEntry {
		t.Helper()
		var got []InventoryEntry
		if err := s.Inventory(context.Background(), bucket, prefix, func(e InventoryEntry) error {
			got = append(got, e)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := collect("", "")
	var keys []string
	for _, e := range all {
		keys = append(keys, e.Bucket+"/"+e.Key+"="+e.Metadata)
	}
	if want := "bkt/a/edit=edited bkt/a/rec=recorded bkt/b-dropped=missing bkt/dir/=folder other/a/z=recorded"; strings.Join(keys, " ") != want {
		t.Errorf("inventory = %s\nwant %s", strings.Join(keys, " "), want)
	}
	rec := all[1]
	if rec.Size != 3 || rec.ETag != `"900150983cd24fb0d6963f7d28e17f72"` || rec.Checksum != "CRC32:Y3Jj SHA256:c2hh" || rec.ContentType != "text/plain" || rec.ModTime.Location().String() != "UTC" {
		t.Errorf("recorded entry = %+v", rec)
	}
	if e := all[0]; e.Size != 3 || !strings.HasSuffix(e.ETag, `-1"`) || e.Checksum != "" {
		t.Errorf("edited entry = %+v", e)
	}
	if got := collect("bkt", "a/"); len(got) != 2 || got[0].Key != "a/edit" {
		t.Errorf("bucket and prefix: %+v", got)
	}

	if err := s.Inventory(context.Background(), "nope", "", nil); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
	stop := errors.New("stop")
	n := 0
	if err := s.Inventory(context.Background(), "", "", func(InventoryEntry) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Errorf("fn error: %v after %d", err, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Inventory(ctx, "", "", func(InventoryEntry) error { t.Error("called after cancel"); return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// The walk's stat can predate a write or a delete the server makes before the record is read.
func TestInventoryEntryLooksAgain(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "old")
	stale, err := os.Stat(filepath.Join(s.root, "bkt", "k"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "k", "replaced")
	if ie, ok := s.inventoryEntry("bkt", listEntry{key: "k", fi: stale}); !ok || ie.Metadata != MetadataRecorded || ie.Size != 8 {
		t.Errorf("replaced meanwhile: %+v %v", ie, ok)
	}
	if err := s.DeleteObject("bkt", "k"); err != nil {
		t.Fatal(err)
	}
	if ie, ok := s.inventoryEntry("bkt", listEntry{key: "k", fi: stale}); ok {
		t.Errorf("deleted meanwhile: %+v", ie)
	}
}

func TestOpenReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bkt"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "bkt", "copied"), []byte("x"), 0o644)
	if os.Geteuid() != 0 {
		_ = os.Chmod(root, 0o555)
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	}
	s, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var got []string
	if err := s.Inventory(context.Background(), "", "", func(e InventoryEntry) error {
		got = append(got, e.Key+"="+e.Metadata)
		return nil
	}); err != nil || strings.Join(got, " ") != "copied=missing" {
		t.Errorf("inventory of a copy: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, SysDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenReadOnly created %s: %v", SysDir, err)
	}
	if _, err := OpenReadOnly(filepath.Join(root, "absent")); err == nil {
		t.Error("opened a missing root")
	}
}
