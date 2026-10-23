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
		if err := s.DeleteObjectIf("bkt", "k", c); !errors.Is(err, ErrPreconditionFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !exists(s, "k") {
		t.Fatal("deleted despite a failed condition")
	}
	if err := s.DeleteObjectIf("bkt", "k", DeleteConditions{IfMatch: info.ETag, Size: 5, HasSize: true, LastModified: mtime}); err != nil {
		t.Errorf("all conditions met: %v", err)
	}
	if exists(s, "k") {
		t.Error("not deleted")
	}
	// A missing object is gone whatever the condition says.
	if err := s.DeleteObjectIf("bkt", "k", DeleteConditions{IfMatch: `"other"`}); err != nil {
		t.Errorf("missing: %v", err)
	}
	if err := s.DeleteObjectIf("bkt", "d/", DeleteConditions{IfMatch: "*", Size: 0, HasSize: true}); err != nil || exists(s, "d/") {
		t.Errorf("marker: %v", err)
	}
	if err := s.DeleteObjectIf("nope", "k", DeleteConditions{IfMatch: "*"}); !errors.Is(err, ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
}
