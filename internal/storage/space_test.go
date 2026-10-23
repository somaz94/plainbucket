package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeDisk reports free bytes for the store's filesystem; err fails the query.
type fakeDisk struct {
	free int64
	err  error
}

func (d *fakeDisk) statfs(string) (int64, int64, error) { return d.free, 1000, d.err }

func TestDiskSpaceReal(t *testing.T) {
	s := newStore(t)
	free, size, err := s.DiskSpace()
	if errors.Is(err, errNoDiskSpace) {
		t.Skip(err)
	}
	if err != nil || free <= 0 || size < free {
		t.Errorf("DiskSpace = %d, %d, %v", free, size, err)
	}
}

func TestMinFree(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 4
	d := &fakeDisk{free: 110}
	s.statfs = d.statfs
	put(t, s, "old", "0123456789")

	// Off by default.
	if _, err := s.PutObject("bkt", "a", strings.NewReader("0123456789"), 10, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	s.SetMinFree(100)
	if s.MinFree() != 100 {
		t.Errorf("MinFree = %d", s.MinFree())
	}
	if _, err := s.PutObject("bkt", "b", strings.NewReader("0123456789"), 10, PutOptions{}); err != nil {
		t.Errorf("write leaving exactly the minimum: %v", err)
	}
	if _, err := s.PutObject("bkt", "c", strings.NewReader("01234567890"), 11, PutOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("write past the minimum: %v", err)
	}
	if exists(s, "c") {
		t.Error("refused write stored")
	}

	// A body without a length needs some room to start.
	if _, err := s.PutObject("bkt", "d", strings.NewReader("x"), -1, PutOptions{}); err != nil {
		t.Errorf("unknown size with room: %v", err)
	}
	d.free = 99
	if _, err := s.PutObject("bkt", "e", strings.NewReader("x"), -1, PutOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("unknown size without room: %v", err)
	}
	d.free = 109
	if _, err := s.CopyObject(context.Background(), "bkt", "old", "bkt", "copy", CopyOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("copy counts the source size: %v", err)
	}
	// A body without a length stops once it passes the room left.
	if _, err := s.PutObject("bkt", "g", strings.NewReader("0123456789"), -1, PutOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("unknown size past the room: %v", err)
	}
	if _, err := s.PutObject("bkt", "g", strings.NewReader("012345678"), -1, PutOptions{}); err != nil {
		t.Errorf("unknown size within the room: %v", err)
	}
	d.free = 99
	if left, _ := os.ReadDir(s.sys("tmp")); len(left) != 0 {
		t.Errorf("staging files left: %d", len(left))
	}

	// Reads and deletes go on.
	if got, _ := read(t, s, "old"); got != "0123456789" {
		t.Errorf("read = %q", got)
	}
	if err := s.DeleteObject("bkt", "a"); err != nil {
		t.Errorf("delete: %v", err)
	}

	// Multipart: each part, and the assembly, which copies the parts once more.
	d.free = 1000
	id, err := s.CreateUpload("bkt", "mp", Metadata{}, MultipartChecksum{})
	if err != nil {
		t.Fatal(err)
	}
	var parts []CompletePart
	for n, body := range []string{"aaaa", "bbbb"} {
		p, err := s.PutPart("bkt", "mp", id, n+1, strings.NewReader(body), 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, CompletePart{Number: n + 1, ETag: p.ETag})
	}
	d.free = 107
	if _, err := s.PutPart("bkt", "mp", id, 3, strings.NewReader("cccc"), 4, nil); err != nil {
		t.Errorf("part leaving the minimum: %v", err)
	}
	d.free = 109
	if _, err := s.PutPartCopy(context.Background(), "bkt", "old", nil, "bkt", "mp", id, 4, CopyOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("part copy: %v", err)
	}
	d.free = 107
	if _, err := s.CompleteUpload(context.Background(), "bkt", "mp", id, parts, Conditions{}, 0, CompleteChecksums{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("complete: %v", err)
	}
	d.free = 108
	if _, err := s.CompleteUpload(context.Background(), "bkt", "mp", id, parts, Conditions{}, 0, CompleteChecksums{}); err != nil {
		t.Errorf("complete with room: %v", err)
	}

	// A write in progress holds its size against the others.
	release, _, err := s.reserve(5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("bkt", "h", strings.NewReader("0123"), 4, PutOptions{}); !errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("beside a reservation: %v", err)
	}
	release()
	if _, err := s.PutObject("bkt", "h", strings.NewReader("0123"), 4, PutOptions{}); err != nil {
		t.Errorf("after the release: %v", err)
	}
	if s.pending.Load() != 0 {
		t.Errorf("pending = %d", s.pending.Load())
	}

	// A filesystem that cannot be queried does not block writes.
	d.err = errors.New("statfs failed")
	if _, err := s.PutObject("bkt", "f", strings.NewReader("x"), 1, PutOptions{}); err != nil {
		t.Errorf("query failing: %v", err)
	}
}
