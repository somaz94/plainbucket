package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTags(t *testing.T) {
	s := newStore(t)
	s.MinPartSize = 1
	tags := []Tag{{"b", "2"}, {"a", "1"}}
	if _, err := s.PutObject("bkt", "k", strings.NewReader("hello"), 5, PutOptions{Metadata: Metadata{ContentType: "text/plain", Tags: tags}}); err != nil {
		t.Fatal(err)
	}
	if _, info := read(t, s, "k"); !reflect.DeepEqual(info.Tags, tags) {
		t.Errorf("put tags = %v", info.Tags)
	}

	// Replacing the tags keeps the data, its mtime and the rest of the metadata.
	before, _ := os.Stat(filepath.Join(s.root, "bkt", "k"))
	if err := s.SetTags("bkt", "k", []Tag{{"c", "3"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(s.root, "bkt", "k"))
	info, _ := s.HeadObject("bkt", "k")
	if !reflect.DeepEqual(info.Tags, []Tag{{"c", "3"}}) || info.ContentType != "text/plain" || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("after SetTags: %+v", info)
	}

	// A copy keeps the source's tags unless ReplaceTags is set, even with ReplaceMetadata.
	ctx := context.Background()
	if _, err := s.CopyObject(ctx, "bkt", "k", "bkt", "same", CopyOptions{ReplaceMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.HeadObject("bkt", "same"); !reflect.DeepEqual(info.Tags, []Tag{{"c", "3"}}) {
		t.Errorf("copied tags = %v", info.Tags)
	}
	if _, err := s.CopyObject(ctx, "bkt", "k", "bkt", "new", CopyOptions{ReplaceTags: true, Tags: []Tag{{"d", "4"}}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.HeadObject("bkt", "new"); !reflect.DeepEqual(info.Tags, []Tag{{"d", "4"}}) {
		t.Errorf("replaced tags = %v", info.Tags)
	}

	// An edit made outside the server keeps the tags, as it keeps the other metadata.
	later := time.Now().Add(time.Minute)
	p := filepath.Join(s.root, "bkt", "k")
	_ = os.WriteFile(p, []byte("edited"), 0o644)
	_ = os.Chtimes(p, later, later)
	if info, _ := s.HeadObject("bkt", "k"); info.Size != 6 || !reflect.DeepEqual(info.Tags, []Tag{{"c", "3"}}) {
		t.Errorf("edited outside: %+v", info)
	}
	_ = os.WriteFile(p, []byte("edited again"), 0o644)
	_ = os.Chtimes(p, later.Add(time.Minute), later.Add(time.Minute))
	hashes := 0
	defer func(orig func(io.Reader) ([]byte, error)) { hashFile = orig }(hashFile)
	hashFile = func(r io.Reader) ([]byte, error) {
		hashes++
		return md5sum("edited again"), nil
	}
	if err := s.SetTags("bkt", "k", []Tag{{"e", "5"}}); err != nil {
		t.Fatal(err)
	}
	if hashes != 1 {
		t.Errorf("hashed %d times", hashes)
	}
	if info, _ := s.HeadObject("bkt", "k"); !reflect.DeepEqual(info.Tags, []Tag{{"e", "5"}}) {
		t.Errorf("SetTags after an outside edit: %+v", info)
	}
	if err := s.SetTags("bkt", "k", nil); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.HeadObject("bkt", "k"); info.Tags != nil {
		t.Errorf("removed tags = %v", info.Tags)
	}

	// Multipart uploads carry their tags to the object; markers keep theirs.
	id, err := s.CreateUpload("bkt", "mp", Metadata{Tags: tags}, MultipartChecksum{})
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := s.PutPart("bkt", "mp", id, 1, strings.NewReader("x"), 1, nil)
	if _, err := s.CompleteUpload(ctx, "bkt", "mp", id, []CompletePart{{Number: 1, ETag: p1.ETag}}, Conditions{}, 0, CompleteChecksums{}); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.HeadObject("bkt", "mp"); !reflect.DeepEqual(info.Tags, tags) {
		t.Errorf("multipart tags = %v", info.Tags)
	}
	if _, err := s.PutObject("bkt", "dir/", strings.NewReader(""), 0, PutOptions{Metadata: Metadata{Tags: tags}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyObject(ctx, "bkt", "dir/", "bkt", "copied/", CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dir/", "copied/"} {
		if info, _ := s.HeadObject("bkt", key); !info.IsDir || !reflect.DeepEqual(info.Tags, tags) {
			t.Errorf("marker %s: %+v", key, info)
		}
	}
	if err := s.SetTags("bkt", "dir/", nil); err != nil {
		t.Fatal(err)
	}

	// A directory that only holds other keys stays a plain prefix.
	put(t, s, "implicit/a", "x")
	for _, key := range []string{"missing", "dir/missing/", "implicit/", "k/"} {
		if err := s.SetTags("bkt", key, tags); !errors.Is(err, ErrNoSuchKey) {
			t.Errorf("%s: %v", key, err)
		}
	}
	if s.hasMarker("bkt", "implicit/") {
		t.Error("tagging a prefix made it a marker")
	}
	if err := s.SetTags("nope", "k", tags); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestSetTagsKeepsLastUse(t *testing.T) {
	s := newStore(t)
	put(t, s, "k", "x")
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	_ = os.Chtimes(s.metaPath("bkt", "k"), old, old)
	if err := s.SetTags("bkt", "k", []Tag{{"a", "1"}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(s.metaPath("bkt", "k")); !fi.ModTime().Equal(old) {
		t.Errorf("sidecar mtime %v, want %v", fi.ModTime(), old)
	}
}
