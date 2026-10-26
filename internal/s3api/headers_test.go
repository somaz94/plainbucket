package s3api

import (
	"net/http"
	"testing"

	"github.com/somaz94/plainbucket/internal/console"
)

func TestHeaderRules(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Console = true
		o.Buckets = map[string]BucketPolicy{"www": {Public: true, Website: &Website{Index: "index.html", Error: "404.html"}, Headers: []HeaderRule{
			{Set: map[string]string{"Cache-Control": "max-age=60", "X-Content-Type-Options": "nosniff"}},
			{Suffix: ".html", Set: map[string]string{"Cache-Control": "no-cache"}},
			{Prefix: "assets/", Set: map[string]string{"Cache-Control": "public, max-age=31536000, immutable"}},
		}}}
	})
	x.mustDo(http.MethodPut, "/www", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/index.html", []byte("<p>hi</p>"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/assets/app.js", []byte("js"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/data.json", []byte("{}"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/own.txt", []byte("x"), map[string]string{"Cache-Control": "private"}, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/dated.txt", []byte("x"), map[string]string{"Expires": "Thu, 01 Jan 2099 00:00:00 GMT"}, http.StatusOK)
	x.mustDo(http.MethodPut, "/www/404.html", []byte("gone"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/plain", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/plain/k", []byte("x"), nil, http.StatusOK)

	for _, c := range []struct{ method, target, want string }{
		{http.MethodGet, "/www/data.json", "max-age=60"},
		{http.MethodGet, "/www/index.html", "no-cache"},
		{http.MethodHead, "/www/index.html", "no-cache"},
		{http.MethodGet, "/www/assets/app.js", "public, max-age=31536000, immutable"},
		{http.MethodGet, "/www/own.txt", "private"},                                    // stored with the object
		{http.MethodGet, "/www/data.json?response-cache-control=no-store", "no-store"}, // the request's override
		// Expires states the same freshness, so it keeps the rule's Cache-Control off.
		{http.MethodGet, "/www/dated.txt", ""},
		{http.MethodGet, "/www/data.json?response-expires=Thu%2C%2001%20Jan%202099%2000%3A00%3A00%20GMT", ""},
		{http.MethodGet, "/plain/k", ""},
	} {
		rec := x.mustDo(c.method, c.target, nil, nil, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s %s: Cache-Control %q, want %q", c.method, c.target, got, c.want)
		}
	}
	if got := x.mustDo(http.MethodGet, "/www/own.txt", nil, nil, http.StatusOK).Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("security header: %q", got)
	}
	etag := x.mustDo(http.MethodHead, "/www/data.json", nil, nil, http.StatusOK).Header().Get("ETag")
	for name, c := range map[string]struct {
		target string
		hdr    map[string]string
		code   int
	}{
		"not modified": {"/www/data.json", map[string]string{"If-None-Match": etag}, http.StatusNotModified},
		"range":        {"/www/data.json", map[string]string{"Range": "bytes=0-0"}, http.StatusPartialContent},
		"part number":  {"/www/data.json?partNumber=1", nil, http.StatusPartialContent},
	} {
		rec := x.mustDo(http.MethodGet, c.target, nil, c.hdr, c.code)
		if rec.Header().Get("Cache-Control") != "max-age=60" {
			t.Errorf("%s: Cache-Control %q", name, rec.Header().Get("Cache-Control"))
		}
	}
	// Rules never touch the sandbox the console puts on everything else its host serves.
	if got := x.mustDo(http.MethodGet, "/www/data.json", nil, nil, http.StatusOK).Header().Get("Content-Security-Policy"); got != console.ObjectSandbox {
		t.Errorf("CSP: %q", got)
	}
	if rec := x.anon(http.MethodGet, "/www/data.json"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "max-age=60" {
		t.Errorf("anonymous read: %d %v", rec.Code, rec.Header())
	}
	// The error document gets its security headers but no freshness, so a 404 is not cached.
	if rec := x.anon(http.MethodGet, "/www/missing.html"); rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("error document: %d %v", rec.Code, rec.Header())
	}
	// An unsigned website read of the root is served the index with its rules.
	if rec := x.anon(http.MethodGet, "/www/"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("website index: %d %v", rec.Code, rec.Header())
	}
}
