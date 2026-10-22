package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLock(t *testing.T) {
	s := newStore(t)
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	other, err := New(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Lock(); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: %v", err)
	}
	_ = s.Close()
	if err := other.Lock(); err != nil {
		t.Errorf("lock after the holder closed: %v", err)
	}
	_ = other.Close()
}

func TestFsck(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	put(t, s, "ok", "fine")
	put(t, s, "edited", "old")
	_ = os.WriteFile(filepath.Join(s.root, "bkt", "edited"), []byte("changed outside"), 0o644)
	_ = os.WriteFile(filepath.Join(s.root, "bkt", "dropped-in"), []byte("new"), 0o644)
	put(t, s, "deleted", "x")
	_ = os.Remove(filepath.Join(s.root, "bkt", "deleted"))
	stale := s.sys("tmp", "upload-old")
	_ = os.WriteFile(stale, nil, 0o600)
	_ = os.Chtimes(stale, c.t.Add(-48*time.Hour), c.t.Add(-48*time.Hour))
	// A key over 1024 bytes is a valid path on Linux but not a valid key. Where the OS
	// limits whole paths to 1024 bytes (macOS) it cannot be created, so it is optional.
	wantName := 0
	seg := strings.Repeat("d", 200)
	deep := filepath.Join(s.root, "bkt", seg, seg, seg, seg, seg)
	if os.MkdirAll(deep, 0o755) == nil && os.WriteFile(filepath.Join(deep, seg), []byte("x"), 0o644) == nil {
		wantName = 1
	}

	run := func(repair bool) map[string][]Finding {
		got := map[string][]Finding{}
		if err := s.Fsck(context.Background(), time.Hour, repair, func(f Finding) { got[f.Kind] = append(got[f.Kind], f) }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := run(false)
	if len(got[ProblemMetadata]) != 2 || len(got[LeftoverMetadata]) != 1 || len(got[LeftoverStaging]) != 1 || len(got[ProblemName]) != wantName {
		t.Errorf("dry run findings = %+v", got)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("dry run removed a staging file")
	}
	got = run(true)
	for kind, fs := range got {
		for _, f := range fs {
			if f.Fixed == (kind == ProblemName) {
				t.Errorf("%s %s: fixed=%v", kind, f.Path, f.Fixed)
			}
		}
	}
	if got := run(false); len(got[ProblemName]) != wantName || len(got) != wantName {
		t.Errorf("after repair = %+v", got)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Fsck(canceled, time.Hour, false, func(Finding) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}
