package checksum

import "testing"

// Expected values are the base64 digests of "hello world" from the AWS SDKs' checksum tests.
func TestNewKnownValues(t *testing.T) {
	cases := map[string]string{
		"CRC32":     "DUoRhQ==",
		"CRC32C":    "yZRlqg==",
		"CRC64NVME": "jSnVw/bqjr4=",
		"SHA1":      "Kq5sNclPz7QV2+lfQIuc6R7oRu0=",
		"SHA256":    "uU0nuZNNPgilLlLX2n2r+sSE7+N6U4DukIj3rOLvzek=",
	}
	for alg, want := range cases {
		h, ok := New(alg)
		if !ok {
			t.Fatalf("%s unsupported", alg)
		}
		h.Write([]byte("hello world"))
		if got := Encode(h.Sum(nil)); got != want {
			t.Errorf("%s = %s, want %s", alg, got, want)
		}
	}
}

func TestNewUnknownAndCase(t *testing.T) {
	if _, ok := New("md5"); ok {
		t.Error("md5 accepted")
	}
	if _, ok := New("crc32c"); !ok {
		t.Error("lower-case name rejected")
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, a := range Algorithms {
		got, ok := FromHeader(Header(a))
		if !ok || got != a {
			t.Errorf("FromHeader(Header(%s)) = %s, %v", a, got, ok)
		}
	}
	if _, ok := FromHeader("x-amz-checksum-md5"); ok {
		t.Error("unknown algorithm accepted")
	}
	if _, ok := FromHeader("content-md5"); ok {
		t.Error("unrelated header accepted")
	}
}
