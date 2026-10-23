package s3api

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

func (x *harness) anon(method, target string) *httptest.ResponseRecorder {
	x.t.Helper()
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(method, "http://pb.test"+target, nil))
	return rec
}

// as runs fn with requests signed by accessKey/secretKey.
func (x *harness) as(accessKey, secretKey string, fn func()) {
	saved := x.signer
	x.signer = sigv4test.Signer{AccessKey: accessKey, SecretKey: secretKey, Region: "us-east-1"}
	defer func() { x.signer = saved }()
	fn()
}

func TestPublicRead(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.PublicBuckets = []string{"pub"} })
	for _, b := range []string{"pub", "priv"} {
		x.mustDo(http.MethodPut, "/"+b, nil, nil, http.StatusOK)
		x.mustDo(http.MethodPut, "/"+b+"/dir/a.txt", []byte("hello"), nil, http.StatusOK)
	}

	if rec := x.anon(http.MethodGet, "/pub/dir/a.txt"); rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Errorf("anonymous GET: %d %q", rec.Code, rec.Body.String())
	}
	if rec := x.anon(http.MethodHead, "/pub/dir/a.txt"); rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "5" {
		t.Errorf("anonymous HEAD: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anon(http.MethodGet, "/pub/missing"); errCode(rec) != "NoSuchKey" {
		t.Errorf("anonymous GET of a missing key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := x.anon(http.MethodGet, "/pub/dir/a.txt?response-content-type=text/html"); errCode(rec) != "InvalidRequest" {
		t.Errorf("anonymous response override: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/priv/dir/a.txt"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/pub"},
		{http.MethodGet, "/pub?list-type=2"},
		{http.MethodGet, "/pub/dir/a.txt?uploadId=x"},
		{http.MethodGet, "/pub/dir/a.txt?partNumber=1"},
		{http.MethodGet, "/pub/dir/a.txt?attributes"},
		{http.MethodPut, "/pub/dir/b.txt"},
		{http.MethodDelete, "/pub/dir/a.txt"},
		{http.MethodPost, "/pub/dir/a.txt?uploads"},
	} {
		if rec := x.anon(c.method, c.target); rec.Code != http.StatusForbidden {
			t.Errorf("anonymous %s %s = %d, want 403", c.method, c.target, rec.Code)
		}
	}
	// A broken presigned URL is reported as a signature problem, not as anonymous.
	if rec := x.anon(http.MethodGet, "/pub/dir/a.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256"); errCode(rec) == "" || rec.Code == http.StatusOK {
		t.Errorf("partial presign: %d %s", rec.Code, rec.Body.String())
	}
}

func TestKeyScopes(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = append(o.Credentials,
			Credential{AccessKey: "RO", SecretKey: "ro-secret", ReadOnly: true},
			Credential{AccessKey: "CACHE", SecretKey: "cache-secret", Buckets: []string{"cache"}})
	})
	for _, b := range []string{"cache", "other"} {
		x.mustDo(http.MethodPut, "/"+b, nil, nil, http.StatusOK)
		x.mustDo(http.MethodPut, "/"+b+"/k", []byte("v"), nil, http.StatusOK)
	}

	x.as("RO", "ro-secret", func() {
		x.mustDo(http.MethodGet, "/other/k", nil, nil, http.StatusOK)
		x.mustDo(http.MethodGet, "/other?list-type=2", nil, nil, http.StatusOK)
		x.expectErr(http.MethodPut, "/other/k2", []byte("v"), nil, "AccessDenied")
		x.expectErr(http.MethodDelete, "/other/k", nil, nil, "AccessDenied")
		x.expectErr(http.MethodPut, "/newbucket", nil, nil, "AccessDenied")
	})

	x.as("CACHE", "cache-secret", func() {
		x.mustDo(http.MethodPut, "/cache/k2", []byte("v"), nil, http.StatusOK)
		x.mustDo(http.MethodGet, "/cache/k", nil, nil, http.StatusOK)
		x.expectErr(http.MethodGet, "/other/k", nil, nil, "AccessDenied")
		x.expectErr(http.MethodGet, "/other?list-type=2", nil, nil, "AccessDenied")
		x.expectErr(http.MethodPut, "/cache/copy", nil, map[string]string{"X-Amz-Copy-Source": "/other/k"}, "AccessDenied")
		x.mustDo(http.MethodPut, "/cache/copy", nil, map[string]string{"X-Amz-Copy-Source": "/cache/k"}, http.StatusOK)

		rec := x.mustDo(http.MethodGet, "/", nil, nil, http.StatusOK)
		var res struct {
			Names []string `xml:"Buckets>Bucket>Name"`
		}
		if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil || strings.Join(res.Names, ",") != "cache" {
			t.Errorf("ListBuckets = %v, %v", res.Names, err)
		}
	})

	x.as("NOPE", "x", func() { x.expectErr(http.MethodGet, "/cache/k", nil, nil, "InvalidAccessKeyId") })
}

// A client that leaves mid-copy gets no 500 and no error log.
func TestCanceledCopyIsNotAnError(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/src", []byte("data"), nil, http.StatusOK)
	r := httptest.NewRequest(http.MethodPut, "http://pb.test/bkt/dst", nil)
	r.Header.Set("X-Amz-Copy-Source", "/bkt/src")
	x.signer.Sign(r, sha(nil), testNow)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r.WithContext(ctx))
	if rec.Body.Len() != 0 || strings.Contains(x.logs.String(), "level=ERROR") || !strings.Contains(x.logs.String(), "request canceled") {
		t.Errorf("canceled copy: %d %q\n%s", rec.Code, rec.Body.String(), x.logs.String())
	}
}

func TestRedactQuery(t *testing.T) {
	got := redactQuery("X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AK%2F20260101&X-Amz-Signature=deadbeef&partNumber=1")
	if strings.Contains(got, "deadbeef") || !strings.Contains(got, "X-Amz-Signature=REDACTED") || !strings.Contains(got, "partNumber=1") {
		t.Errorf("redacted = %q", got)
	}
	if redactQuery("%zz") != "" {
		t.Error("malformed query not dropped")
	}
}

func TestSetAccess(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/pub", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/pub/k", []byte("x"), nil, http.StatusOK)
	x.as("new", "new-secret", func() { x.expectErr(http.MethodGet, "/", nil, nil, "InvalidAccessKeyId") })
	if rec := x.anon(http.MethodGet, "/pub/k"); rec.Code != http.StatusForbidden {
		t.Errorf("private before: %d", rec.Code)
	}

	x.h.SetAccess([]Credential{{AccessKey: "new", SecretKey: "new-secret"}}, nil, []string{"pub"})
	x.as("new", "new-secret", func() { x.mustDo(http.MethodGet, "/", nil, nil, http.StatusOK) })
	x.expectErr(http.MethodGet, "/", nil, nil, "InvalidAccessKeyId")
	if rec := x.anon(http.MethodGet, "/pub/k"); rec.Code != http.StatusOK {
		t.Errorf("public after: %d", rec.Code)
	}
}
