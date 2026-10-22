package s3api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func websiteHarness(t *testing.T) *harness {
	t.Helper()
	x := newHarness(t, func(o *Options) {
		o.Domains = []string{"example.com"}
		o.Buckets = map[string]BucketPolicy{
			"www":  {Public: true, Website: &Website{Index: "index.html", Error: "404.html"}},
			"bare": {Public: true, Website: &Website{Index: "index.html"}},
		}
	})
	for _, b := range []string{"www", "bare"} {
		x.mustDo(http.MethodPut, "/"+b, nil, nil, http.StatusOK)
		x.mustDo(http.MethodPut, "/"+b+"/index.html", []byte("<h1>home</h1>"), map[string]string{"Content-Type": "text/html"}, http.StatusOK)
		x.mustDo(http.MethodPut, "/"+b+"/docs/index.html", []byte("docs"), nil, http.StatusOK)
	}
	x.mustDo(http.MethodPut, "/www/404.html", []byte("not here"), map[string]string{"Content-Type": "text/html"}, http.StatusOK)
	return x
}

func (x *harness) anonHost(method, host, target string, hdr map[string]string) *httptest.ResponseRecorder {
	x.t.Helper()
	r := httptest.NewRequest(method, "http://"+host+target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func TestWebsite(t *testing.T) {
	x := websiteHarness(t)
	for _, c := range []struct{ host, target, body string }{
		{"pb.test", "/www/", "<h1>home</h1>"},
		{"pb.test", "/www", "<h1>home</h1>"},
		{"www.example.com", "/", "<h1>home</h1>"},
		{"www.example.com", "/docs/", "docs"},
		{"pb.test", "/www/docs/", "docs"},
	} {
		rec := x.anonHost(http.MethodGet, c.host, c.target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != c.body {
			t.Errorf("GET %s%s = %d %q", c.host, c.target, rec.Code, rec.Body.String())
		}
	}
	if rec := x.anonHost(http.MethodGet, "www.example.com", "/docs", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docs/" {
		t.Errorf("redirect to the prefix: %d %v", rec.Code, rec.Header())
	}
	rec := x.anonHost(http.MethodGet, "www.example.com", "/missing.png", nil)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "not here" || rec.Header().Get("Content-Type") != "text/html" {
		t.Errorf("error document: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec := x.anonHost(http.MethodHead, "www.example.com", "/missing.png", nil); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Errorf("HEAD error document: %d %q", rec.Code, rec.Body.String())
	}
	if rec := x.anonHost(http.MethodGet, "bare.example.com", "/missing", nil); rec.Code != http.StatusNotFound || errCode(rec) != "NoSuchKey" {
		t.Errorf("no error document: %d %s", rec.Code, rec.Body.String())
	}
	// Signed requests keep S3 semantics: the root lists, a missing key is NoSuchKey.
	if rec := x.doHost("www.example.com", http.MethodGet, "/?list-type=2", nil); rec.Code != http.StatusOK || errCode(rec) != "" {
		t.Errorf("signed listing: %d %s", rec.Code, rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/www/missing.png", nil, nil, "NoSuchKey")
	// Writes stay forbidden.
	if rec := x.anonHost(http.MethodPut, "www.example.com", "/x", nil); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous PUT: %d", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Buckets = map[string]BucketPolicy{
			"web": {Public: true, CORS: &CORS{
				AllowedOrigins: []string{"https://app.example.com", "https://*.preview.example.com"},
				AllowedMethods: []string{"GET", "HEAD", "PUT"},
				AllowedHeaders: []string{"Authorization", "Content-Type", "X-Amz-*"},
				ExposeHeaders:  []string{"ETag"},
				MaxAge:         time.Hour,
			}},
			"star": {CORS: &CORS{AllowedOrigins: []string{"*"}, AllowedHeaders: []string{"*"}}},
		}
	})
	x.mustDo(http.MethodPut, "/web", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/web/a.txt", []byte("a"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/plain", nil, nil, http.StatusOK)

	rec := x.anonHost(http.MethodGet, "pb.test", "/web/a.txt", map[string]string{"Origin": "https://app.example.com"})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		rec.Header().Get("Access-Control-Expose-Headers") != "ETag" || rec.Header().Get("Vary") != "Origin" {
		t.Errorf("simple GET: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anonHost(http.MethodGet, "pb.test", "/web/missing", map[string]string{"Origin": "https://app.example.com"}); rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("CORS headers missing on an error response")
	}
	if rec := x.anonHost(http.MethodGet, "pb.test", "/web/a.txt", map[string]string{"Origin": "https://evil.example"}); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS granted to an unlisted origin")
	}

	pre := func(bucket, origin, method, headers string) *httptest.ResponseRecorder {
		h := map[string]string{"Origin": origin, "Access-Control-Request-Method": method}
		if headers != "" {
			h["Access-Control-Request-Headers"] = headers
		}
		return x.anonHost(http.MethodOptions, "pb.test", "/"+bucket+"/a.txt", h)
	}
	rec = pre("web", "https://pr-7.preview.example.com", "PUT", "content-type, x-amz-date")
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD, PUT" ||
		rec.Header().Get("Access-Control-Max-Age") != "3600" || rec.Header().Get("Access-Control-Allow-Headers") != "content-type, x-amz-date" {
		t.Errorf("preflight: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	for _, c := range []struct{ bucket, origin, method, headers string }{
		{"web", "https://evil.example", "GET", ""},
		{"web", "https://app.example.com", "DELETE", ""},
		{"web", "https://app.example.com", "PUT", "x-custom"},
	} {
		if rec := pre(c.bucket, c.origin, c.method, c.headers); rec.Code != http.StatusForbidden {
			t.Errorf("preflight %+v = %d", c, rec.Code)
		}
	}
	if rec := pre("star", "https://anything.example", "GET", "x-whatever"); rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://anything.example" {
		t.Errorf("wildcard preflight: %d %v", rec.Code, rec.Header())
	}
	// Without a CORS rule, OPTIONS is not special.
	if rec := pre("plain", "https://app.example.com", "GET", ""); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("no CORS rule: %d %v", rec.Code, rec.Header())
	}
}
