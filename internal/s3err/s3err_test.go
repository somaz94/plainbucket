package s3err

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	NoSuchKey().Write(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil), "REQ1")
	body := rec.Body.String()
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/xml" ||
		!strings.Contains(body, "<Code>NoSuchKey</Code>") || !strings.Contains(body, "<Resource>/b/k</Resource>") ||
		!strings.Contains(body, "<RequestId>REQ1</RequestId>") {
		t.Errorf("%d %s", rec.Code, body)
	}
	rec = httptest.NewRecorder()
	AccessDenied("x").Write(rec, httptest.NewRequest(http.MethodHead, "/b/k", nil), "REQ2")
	if rec.Code != http.StatusForbidden || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", rec.Code, rec.Body.String())
	}
	if got := SignatureDoesNotMatch().Error(); !strings.HasPrefix(got, "SignatureDoesNotMatch: ") {
		t.Errorf("Error() = %q", got)
	}
}

func TestConstructorsHaveStatusAndCode(t *testing.T) {
	for _, e := range []*Error{
		AccessDenied(""), AuthHeaderMalformed(""), AuthQueryMalformed(""), InvalidAccessKeyID(), SignatureDoesNotMatch(),
		RequestTimeTooSkewed(), ContentSHA256Mismatch(), BadDigest(""), InvalidDigest(), InvalidArgument(""),
		InvalidRequest(""), MalformedXML(), IncompleteBody(), MissingContentLength(), EntityTooLarge(), EntityTooSmall(),
		InvalidBucketName(), KeyTooLong(), InvalidKey(), KeyConflict(), NoSuchBucket(), NoSuchKey(), NoSuchUpload(),
		BucketAlreadyOwnedByYou(), BucketNotEmpty(), InvalidPart(), InvalidPartOrder(), PreconditionFailed(),
		NotImplemented(), MethodNotAllowed(), Internal(), QuotaExceeded(),
	} {
		if e.Status < 400 || e.Code == "" {
			t.Errorf("%+v", e)
		}
	}
}
