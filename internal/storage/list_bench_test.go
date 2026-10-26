package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchBucket writes n files per directory over dirs directories (0: all at the top)
// straight to disk, so they carry no sidecar.
func benchBucket(b *testing.B, dirs, n int) *Store {
	b.Helper()
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err := s.CreateBucket("bkt"); err != nil {
		b.Fatal(err)
	}
	write := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for i := range n {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%06d", i)), []byte("x"), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	if dirs == 0 {
		write(s.bucketPath("bkt"))
	}
	for d := range dirs {
		write(filepath.Join(s.bucketPath("bkt"), fmt.Sprintf("dir-%04d", d)))
	}
	return s
}

// listPages pages through the whole bucket as a client does and returns the count of
// objects and common prefixes.
func listPages(b *testing.B, s *Store, p ListParams) int {
	b.Helper()
	n := 0
	for {
		res, err := s.List("bkt", p)
		if err != nil {
			b.Fatal(err)
		}
		n += len(res.Objects) + len(res.CommonPrefixes)
		if !res.Truncated {
			return n
		}
		p.After = res.Next
	}
}

func BenchmarkListFlat(b *testing.B) {
	s := benchBucket(b, 0, 20000)
	for b.Loop() {
		if got := listPages(b, s, ListParams{MaxKeys: 1000}); got != 20000 {
			b.Fatal(got)
		}
	}
}

func BenchmarkListNested(b *testing.B) {
	s := benchBucket(b, 20, 1000)
	for b.Loop() {
		if got := listPages(b, s, ListParams{MaxKeys: 1000}); got != 20000 {
			b.Fatal(got)
		}
	}
}

func BenchmarkListFirstPageFlat(b *testing.B) {
	s := benchBucket(b, 0, 20000)
	for b.Loop() {
		if _, err := s.List("bkt", ListParams{MaxKeys: 1000}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListDelimiter(b *testing.B) {
	s := benchBucket(b, 20, 1000)
	for b.Loop() {
		if got := listPages(b, s, ListParams{MaxKeys: 1000, Delimiter: "/"}); got != 20 {
			b.Fatal(got)
		}
	}
}
