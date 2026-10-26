package storage

import (
	"errors"
	"testing"
	"time"
)

func TestDeleteObjectIf(t *testing.T) {
	s := newStore(t)
	info := put(t, s, "k", "hello")
	put(t, s, "d/", "")
	mtime := info.ModTime.Truncate(time.Second) // as HTTP dates carry it
	for name, c := range map[string]DeleteConditions{
		"etag":  {IfMatch: `"other"`},
		"size":  {Size: 4, HasSize: true},
		"mtime": {LastModified: mtime.Add(time.Second)},
	} {
		if _, err := s.DeleteObjectIf("bkt", "k", c); !errors.Is(err, ErrPreconditionFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !exists(s, "k") {
		t.Fatal("deleted despite a failed condition")
	}
	if removed, err := s.DeleteObjectIf("bkt", "k", DeleteConditions{IfMatch: info.ETag, Size: 5, HasSize: true, LastModified: mtime}); err != nil || !removed {
		t.Errorf("all conditions met: %v %v", removed, err)
	}
	if exists(s, "k") {
		t.Error("not deleted")
	}
	// A missing object is gone whatever the condition says.
	if removed, err := s.DeleteObjectIf("bkt", "k", DeleteConditions{IfMatch: `"other"`}); err != nil || removed {
		t.Errorf("missing: %v %v", removed, err)
	}
	if removed, err := s.DeleteObjectIf("bkt", "k", DeleteConditions{}); err != nil || removed {
		t.Errorf("missing, unconditional: %v %v", removed, err)
	}
	if removed, err := s.DeleteObjectIf("bkt", "d/", DeleteConditions{IfMatch: "*", Size: 0, HasSize: true}); err != nil || !removed || exists(s, "d/") {
		t.Errorf("marker: %v %v", removed, err)
	}
	if removed, err := s.DeleteObjectIf("bkt", "d/", DeleteConditions{}); err != nil || removed {
		t.Errorf("missing marker: %v %v", removed, err)
	}
	// A file named like the marker without its slash is another object.
	put(t, s, "f", "file")
	if removed, err := s.DeleteObjectIf("bkt", "f/", DeleteConditions{}); err != nil || removed || !exists(s, "f") {
		t.Errorf("marker delete over a file: %v %v, file kept %v", removed, err, exists(s, "f"))
	}
	// A folder with objects in it loses its marker and keeps the objects.
	put(t, s, "e/", "")
	put(t, s, "e/x", "x")
	if removed, err := s.DeleteObjectIf("bkt", "e/", DeleteConditions{}); err != nil || !removed || s.hasMarker("bkt", "e/") || !exists(s, "e/x") {
		t.Errorf("marker of a non-empty folder: %v %v", removed, err)
	}
	if removed, err := s.DeleteObjectIf("bkt", "bad//key", DeleteConditions{}); err != nil || removed {
		t.Errorf("invalid key: %v %v", removed, err)
	}
	if _, err := s.DeleteObjectIf("nope", "k", DeleteConditions{IfMatch: "*"}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
}
