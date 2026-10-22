package s3api

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doHost signs and serves a request sent to host.
func (x *harness) doHost(host, method, target string, body []byte) *httptest.ResponseRecorder {
	x.t.Helper()
	r := httptest.NewRequest(method, "http://"+host+target, bytes.NewReader(body))
	x.signer.Sign(r, sha(body), testNow)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func TestVirtualHostedStyle(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Domains = []string{"example.com", "s3.example.com"}
		o.PublicBuckets = []string{"site"}
	})
	rec := x.doHost("site.s3.example.com:9000", http.MethodPut, "/", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "http://site.s3.example.com:9000/" {
		t.Fatalf("create: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := x.doHost("site.s3.example.com:9000", http.MethodPut, "/a/b.txt", []byte("hi")); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	// The same object path-style, through the domain itself and through any other host.
	x.mustDo(http.MethodGet, "/site/a/b.txt", nil, nil, http.StatusOK)
	if rec := x.doHost("s3.example.com", http.MethodGet, "/site/a/b.txt", nil); rec.Code != http.StatusOK {
		t.Errorf("path-style on the domain: %d", rec.Code)
	}
	// The longest domain wins: under example.com this host would name bucket site.s3.
	if rec := x.doHost("SITE.S3.Example.com", http.MethodGet, "/a/b.txt", nil); rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Errorf("vhost get: %d %q", rec.Code, rec.Body.String())
	}
	rec = x.doHost("site.s3.example.com", http.MethodGet, "/?list-type=2", nil)
	var list struct {
		Keys []string `xml:"Contents>Key"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Keys) != 1 || list.Keys[0] != "a/b.txt" {
		t.Errorf("vhost list: %d %v %s", rec.Code, err, rec.Body.String())
	}

	// Anonymous reads of a public bucket work through its virtual host, and /healthz there
	// is an object key, not the health endpoint.
	anon := func(host, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+target, nil)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec
	}
	if rec := anon("site.s3.example.com", "/a/b.txt"); rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Errorf("anonymous vhost get: %d %q", rec.Code, rec.Body.String())
	}
	if rec := anon("site.s3.example.com", "/healthz"); errCode(rec) != "NoSuchKey" {
		t.Errorf("vhost /healthz: %d %s", rec.Code, rec.Body.String())
	}
	if rec := anon("s3.example.com", "/healthz"); rec.Body.String() != "ok" {
		t.Errorf("healthz on the domain: %d %q", rec.Code, rec.Body.String())
	}
	// A label that is not a valid bucket name falls back to path-style.
	if rec := x.doHost("Bad_Name.example.com", http.MethodGet, "/site/a/b.txt", nil); rec.Code != http.StatusOK {
		t.Errorf("invalid vhost label: %d %s", rec.Code, rec.Body.String())
	}

	// CompleteMultipartUpload reports the object's URL in the addressing it was sent with.
	host := "site.s3.example.com"
	rec = x.doHost(host, http.MethodPost, "/mp?uploads", nil)
	var init struct{ UploadId string }
	_ = xml.Unmarshal(rec.Body.Bytes(), &init)
	part := x.doHost(host, http.MethodPut, "/mp?partNumber=1&uploadId="+init.UploadId, []byte("data"))
	doc := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part.Header().Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`)
	rec = x.doHost(host, http.MethodPost, "/mp?uploadId="+init.UploadId, doc)
	var done struct{ Location string }
	if err := xml.Unmarshal(rec.Body.Bytes(), &done); err != nil || done.Location != "http://site.s3.example.com/mp" {
		t.Errorf("complete: %d %+v %s", rec.Code, done, rec.Body.String())
	}
}

func TestNoDomainsMeansPathStyle(t *testing.T) {
	x := newHarness(t, nil)
	if _, ok := x.h.vhostBucket("bkt.example.com"); ok {
		t.Error("vhost routing without configured domains")
	}
}

// A configured domain that is also a subdomain of another must stay path-style, or
// api.example.com would read as bucket "api" of example.com.
func TestNestedDomainIsPathStyle(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.Domains = []string{"example.com", "api.example.com"} })
	x.mustDo(http.MethodPut, "/site", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/site/a.txt", []byte("hi"), nil, http.StatusOK)
	if rec := x.doHost("api.example.com", http.MethodGet, "/site/a.txt", nil); rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Errorf("path-style on the nested domain: %d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://api.example.com/healthz", nil))
	if rec.Body.String() != "ok" {
		t.Errorf("healthz on the nested domain: %d %q", rec.Code, rec.Body.String())
	}
	if rec := x.doHost("site.api.example.com", http.MethodGet, "/a.txt", nil); rec.Code != http.StatusOK {
		t.Errorf("vhost under the nested domain: %d %s", rec.Code, rec.Body.String())
	}
}
