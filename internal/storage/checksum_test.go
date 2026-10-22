package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// The expected values are those ceph/s3-tests checks for three 5 MiB parts of 'A', 'B'
// and 'C' (test_multipart_use_cksum_helper_*).
func TestObjectChecksumVectors(t *testing.T) {
	const size = 5 << 20
	for _, c := range []struct{ alg, typ, want string }{
		{"CRC32", ChecksumFullObject, "WgDhBQ=="},
		{"CRC32C", ChecksumFullObject, "xU+Krw=="},
		{"CRC64NVME", ChecksumFullObject, "i+6LR0y3eFo="},
		{"SHA1", ChecksumComposite, "sizjvY4eud3MrcHdZM3cQ/ol39o=-3"},
		{"SHA256", ChecksumComposite, "uWBwpe1dxI4Vw8Gf0X9ynOdw/SS6VBzfWm9giiv1sf4=-3"},
	} {
		sum, err := newObjectChecksum(MultipartChecksum{Algorithm: c.alg, Type: c.typ}, nil, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range []byte("ABC") {
			if _, err := io.Copy(sum.part(io.Discard), bytes.NewReader(bytes.Repeat([]byte{b}, size))); err != nil {
				t.Fatal(err)
			}
			sum.endPart()
		}
		if got := sum.value(); got != c.want {
			t.Errorf("%s %s = %s, want %s", c.alg, c.typ, got, c.want)
		}
	}
}

func TestNewObjectChecksum(t *testing.T) {
	if c, err := newObjectChecksum(MultipartChecksum{}, nil, 1); c != nil || err != nil {
		t.Errorf("no checksum: %v %v", c, err)
	}
	c, _ := newObjectChecksum(MultipartChecksum{}, map[string]string{"SHA256": "x-2"}, 2)
	if c.alg != "SHA256" || c.typ != ChecksumComposite {
		t.Errorf("declared composite: %+v", c)
	}
	c, _ = newObjectChecksum(MultipartChecksum{}, map[string]string{"CRC32": "x"}, 2)
	if c.typ != ChecksumFullObject {
		t.Errorf("declared full object: %+v", c)
	}
	if c, _ := newObjectChecksum(MultipartChecksum{Algorithm: "crc64nvme"}, nil, 1); c.typ != ChecksumFullObject {
		t.Errorf("CRC64NVME default type: %+v", c)
	}
	for _, c := range []struct {
		up       MultipartChecksum
		declared map[string]string
	}{
		{MultipartChecksum{}, map[string]string{"SHA1": "x"}},
		{MultipartChecksum{}, map[string]string{"CRC32": "x", "CRC32C": "y"}},
		{MultipartChecksum{Algorithm: "SHA256"}, map[string]string{"CRC32": "x"}},
	} {
		if _, err := newObjectChecksum(c.up, c.declared, 1); !errors.Is(err, ErrInvalidChecksum) {
			t.Errorf("%+v %v: %v", c.up, c.declared, err)
		}
	}
	if c.part(nil) == nil {
		t.Error("part writer")
	}
	var none *objectChecksum
	if w := none.part(io.Discard); w != io.Discard {
		t.Error("nil checksum passes the writer through")
	}
	none.endPart()

	for _, v := range []struct {
		alg, typ string
		ok       bool
	}{
		{"CRC32", ChecksumComposite, true}, {"crc32c", ChecksumFullObject, true},
		{"SHA256", ChecksumFullObject, false}, {"CRC64NVME", ChecksumComposite, false}, {"MD5", ChecksumComposite, false},
	} {
		if got := ValidChecksumType(v.alg, v.typ); got != v.ok {
			t.Errorf("ValidChecksumType(%s, %s) = %v", v.alg, v.typ, got)
		}
	}
}

func TestCompleteUploadChecksums(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	upload := func(key string, ck MultipartChecksum) (string, PartInfo) {
		t.Helper()
		id, err := s.CreateUpload("bkt", key, Metadata{}, ck)
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.PutPart("bkt", key, id, 1, strings.NewReader(strings.Repeat("A", 1024)), 1024, func([]byte) (map[string]string, error) {
			return map[string]string{"SHA256": "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0="}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, p
	}
	// test_multipart_checksum_sha256's composite of one part.
	const composite = "Ok6Cs5b96ux6+MWQkJO7UBT5sKPBeXBLwvj/hK89smg=-1"
	sha := MultipartChecksum{Algorithm: "SHA256"}

	id, p := upload("bad", sha)
	if _, err := s.CompleteUpload(ctx, "bkt", "bad", id, []CompletePart{{1, p.ETag}}, Conditions{}, 0,
		CompleteChecksums{Object: map[string]string{"SHA256": "bad"}}); !errors.Is(err, ErrBadDigest) {
		t.Errorf("wrong object checksum: %v", err)
	}
	if _, err := s.CompleteUpload(ctx, "bkt", "bad", id, []CompletePart{{1, p.ETag}}, Conditions{}, 0,
		CompleteChecksums{Parts: map[int]map[string]string{1: {"SHA256": "other"}}}); !errors.Is(err, ErrInvalidPart) {
		t.Errorf("wrong part checksum: %v", err)
	}
	if _, err := s.CompleteUpload(ctx, "bkt", "bad", id, []CompletePart{{1, p.ETag}}, Conditions{}, 0,
		CompleteChecksums{Parts: map[int]map[string]string{1: {"CRC32": "x"}}}); !errors.Is(err, ErrInvalidPart) {
		t.Errorf("part checksum the part never had: %v", err)
	}

	id, p = upload("good", sha)
	info, err := s.CompleteUpload(ctx, "bkt", "good", id, []CompletePart{{1, p.ETag}}, Conditions{}, 0,
		CompleteChecksums{Object: map[string]string{"SHA256": composite}, Parts: map[int]map[string]string{1: p.Checksums}})
	if err != nil || info.Checksums["SHA256"] != composite || info.ChecksumType != ChecksumComposite {
		t.Fatalf("complete: %+v %v", info, err)
	}
	if len(info.Parts) != 1 || info.Parts[0].Size != 1024 || info.Parts[0].Checksums["SHA256"] == "" {
		t.Errorf("parts: %+v", info.Parts)
	}
	head, err := s.HeadObject("bkt", "good")
	if err != nil || head.ChecksumType != ChecksumComposite || len(head.Parts) != 1 {
		t.Errorf("head keeps the checksum type and parts: %+v %v", head, err)
	}

	cp, err := s.CopyObject(ctx, "bkt", "good", "bkt", "copy", CopyOptions{})
	if err != nil || cp.Checksums != nil || cp.Parts != nil {
		t.Errorf("a copy drops the composite checksum and parts: %+v %v", cp, err)
	}
}
