package s3api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperation(t *testing.T) {
	cases := []struct {
		method, target, copySource, want string
	}{
		{http.MethodGet, "/", "", "ListBuckets"},
		{http.MethodPut, "/", "", "Unknown"},
		{http.MethodOptions, "/b/k", "", "Preflight"},
		{http.MethodGet, "/b?location", "", "GetBucketLocation"},
		{http.MethodGet, "/b?versioning", "", "GetBucketVersioning"},
		{http.MethodGet, "/b?uploads", "", "ListMultipartUploads"},
		{http.MethodGet, "/b?versions", "", "ListObjectVersions"},
		{http.MethodGet, "/b?list-type=2", "", "ListObjectsV2"},
		{http.MethodGet, "/b?list-type=3", "", "Unknown"},
		{http.MethodGet, "/b", "", "ListObjects"},
		{http.MethodHead, "/b", "", "HeadBucket"},
		{http.MethodPut, "/b", "", "CreateBucket"},
		{http.MethodPut, "/b?versioning", "", "NotImplemented"},
		{http.MethodDelete, "/b", "", "DeleteBucket"},
		{http.MethodPost, "/b?delete", "", "DeleteObjects"},
		{http.MethodPost, "/b", "", "Unknown"},
		{http.MethodGet, "/b?policy", "", "NotImplemented"},
		{http.MethodGet, "/b/k", "", "GetObject"},
		{http.MethodHead, "/b/k", "", "HeadObject"},
		{http.MethodGet, "/b/k?tagging", "", "GetObjectTagging"},
		{http.MethodGet, "/b/k?attributes", "", "GetObjectAttributes"},
		{http.MethodGet, "/b/k?uploadId=u", "", "ListParts"},
		{http.MethodGet, "/b/k?versionId=null", "", "GetObject"},
		{http.MethodGet, "/b/k?versionId=v1", "", "NotImplemented"},
		{http.MethodPut, "/b/k?tagging", "", "NotImplemented"},
		{http.MethodPut, "/b/k?versioning", "", "NotImplemented"},
		{http.MethodPut, "/b/k", "", "PutObject"},
		{http.MethodPut, "/b/k", "/src/k", "CopyObject"},
		{http.MethodPut, "/b/k?partNumber=1&uploadId=u", "", "UploadPart"},
		{http.MethodPut, "/b/k?partNumber=1&uploadId=u", "/src/k", "UploadPartCopy"},
		{http.MethodDelete, "/b/k", "", "DeleteObject"},
		{http.MethodDelete, "/b/k?uploadId=u", "", "AbortMultipartUpload"},
		{http.MethodPost, "/b/k?uploads", "", "CreateMultipartUpload"},
		{http.MethodPost, "/b/k?uploadId=u", "", "CompleteMultipartUpload"},
		{http.MethodPost, "/b/k", "", "Unknown"},
		{http.MethodPatch, "/b/k", "", "Unknown"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://pb.test"+c.target, nil)
		if c.copySource != "" {
			r.Header.Set("X-Amz-Copy-Source", c.copySource)
		}
		bucket, key := "", ""
		if p := r.URL.Path[1:]; p != "" {
			bucket, key, _ = strings.Cut(p, "/")
		}
		if got := operation(r, bucket, key); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.method, c.target, got, c.want)
		}
	}
}

func TestOperationOfPostUploads(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://pb.test/b", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if got := operation(r, "b", ""); got != "PostObject" {
		t.Errorf("form post: %s", got)
	}
	r = httptest.NewRequest(http.MethodPost, "http://pb.test/b?delete", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if got := operation(r, "b", ""); got != "DeleteObjects" {
		t.Errorf("delete with a form type: %s", got)
	}
}

func TestOperationOfWebsiteRequests(t *testing.T) {
	x := websiteHarness(t)
	x.h.opts.Metrics = true
	x.anonHost(http.MethodGet, "www.example.com", "/", nil)
	x.anonHost(http.MethodHead, "www.example.com", "/docs/", nil)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`plainbucket_requests_total{operation="GetObject",method="GET",status="200"} 1`,
		`plainbucket_requests_total{operation="HeadObject",method="HEAD",status="200"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, rec.Body.String())
		}
	}
}
