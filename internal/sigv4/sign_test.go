package sigv4

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

func TestSign_AWSDocVector(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	Signer{AccessKey: docAK, SecretKey: docSK, Region: "us-east-1"}.Sign(r, EmptySHA256, docNow.Add(-5*time.Minute))
	want := "AWS4-HMAC-SHA256 Credential=" + docAK + "/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

// The independent test signer and Verify must agree with Sign on keys that need encoding.
func TestSignAgreesWithTestSigner(t *testing.T) {
	for _, key := range []string{"plain.txt", "a b/c+d=e&f", "한글/파일 (1).txt", "x~y_z-.", "%41"} {
		target := "http://pb.test/bkt/" + uriEncode(key, false) + "?partNumber=2&uploadId=a%2Bb"
		r := httptest.NewRequest(http.MethodPut, target, strings.NewReader("data"))
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("X-Amz-Meta-Note", "  two   spaces ")
		Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}.Sign(r, UnsignedPayload, docNow)
		if _, e := Verify(r, func(k string) (string, bool) { return "SK", k == "AK" }, docNow); e != nil {
			t.Errorf("%q: Verify: %v", key, e)
		}

		// The test signer signs host and x-amz-* only, so compare on a request with no others.
		mine := httptest.NewRequest(http.MethodPut, target, nil)
		mine.Header.Set("X-Amz-Meta-Note", "  two   spaces ")
		Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}.Sign(mine, UnsignedPayload, docNow)
		ref := httptest.NewRequest(http.MethodPut, target, nil)
		ref.Header.Set("X-Amz-Meta-Note", "two spaces")
		sig := sigv4test.Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}.Sign(ref, UnsignedPayload, docNow)
		if got := mine.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+sig) {
			t.Errorf("%q: %s, want signature %s", key, got, sig)
		}
	}
}
