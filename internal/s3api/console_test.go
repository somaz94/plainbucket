package s3api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/console"
	"github.com/somaz94/plainbucket/internal/storage"
)

func TestConsoleDisabled(t *testing.T) {
	x := newHarness(t, nil)
	if rec := x.anon(http.MethodGet, "/_console/"); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("console served while disabled: %d %s", rec.Code, rec.Body.String())
	}
	if rec := x.do(http.MethodGet, consoleUsagePath, nil, nil); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "buckets") {
		t.Errorf("usage API served while disabled: %d %s", rec.Code, rec.Body.String())
	}
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	if rec := x.mustDo(http.MethodGet, "/bkt", nil, nil, http.StatusOK); rec.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("sandbox without the console: %v", rec.Header())
	}
}

func TestConsoleFiles(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.Console = true; o.Domains = []string{"s3.test"}; o.AccessLog = true })
	rec := x.anon(http.MethodGet, "/_console/")
	h := rec.Header()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "media-src 'self'") ||
		h.Get("Cross-Origin-Opener-Policy") != "same-origin" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("index: %d %v %s", rec.Code, h, rec.Body.String())
	}
	if !strings.Contains(x.logs.String(), `path=/_console/`) || !strings.Contains(x.logs.String(), `bucket="" key=""`) {
		t.Errorf("console request logged as an object:\n%s", x.logs.String())
	}
	if rec := x.anon(http.MethodHead, "/_console/"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("head: %d %q", rec.Code, rec.Body.String())
	}
	if rec := x.anon(http.MethodGet, "/_console"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/_console/" {
		t.Errorf("redirect: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anon(http.MethodPost, "/_console"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("post to the redirect: %d", rec.Code)
	}
	if rec := x.anon(http.MethodGet, "/_console/index.html"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "./" {
		t.Errorf("index.html: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anon(http.MethodGet, "/_console/app.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Errorf("script: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anon(http.MethodPut, "/_console/app.js"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("put: %d", rec.Code)
	}
	for _, p := range []string{"/_console/missing.js", "/_console/../go.mod", "/_console/%2e%2e/go.mod", "/_console/..%2fconsole.go"} {
		if rec := x.anon(http.MethodGet, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}

	// Objects on the console's host are sandboxed; on a virtual host the path is an
	// object key of that bucket, not the console, and the bucket has its own origin.
	x.mustDo(http.MethodPut, "/site", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/site/page.html", []byte("<p>hi</p>"), nil, http.StatusOK)
	if rec := x.mustDo(http.MethodGet, "/site/page.html", nil, nil, http.StatusOK); rec.Header().Get("Content-Security-Policy") != console.ObjectSandbox {
		t.Errorf("path-style object: %v", rec.Header())
	}
	r := httptest.NewRequest(http.MethodGet, "http://site.s3.test/page.html", nil)
	x.signer.Sign(r, sha(nil), testNow)
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("virtual-hosted object: %d %v", rec.Code, rec.Header())
	}
	if rec := x.anonHost(http.MethodGet, "site.s3.test", "/_console/", nil); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "app.js") {
		t.Errorf("console on a virtual host: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConsoleUsage(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Console = true
		o.Buckets = map[string]BucketPolicy{"bbb": {Public: true, Website: &Website{Index: "index.html"}, CORS: &CORS{AllowedOrigins: []string{"*"}}}}
		o.Credentials = append(o.Credentials, Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"aaa"}},
			Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true})
	})
	x.h.store.SetLimits(map[string]storage.Limit{"aaa": {MaxSize: 100, Evict: true, ExpireAfter: 336 * time.Hour}})
	for _, b := range []string{"aaa", "bbb"} {
		x.mustDo(http.MethodPut, "/"+b, nil, nil, http.StatusOK)
	}
	x.mustDo(http.MethodPut, "/aaa/k", []byte("123"), nil, http.StatusOK)
	if body := x.mustDo(http.MethodGet, consoleUsagePath, nil, nil, http.StatusOK).Body.String(); strings.Contains(body, "measured_at") ||
		!strings.Contains(body, `"total":{"objects":1,"bytes":3}`) || strings.Contains(body, `"name":""`) {
		t.Errorf("before the first measurement: %s", body)
	}
	x.h.store.MeasureUsage(t.Context())

	get := func() consoleUsageResponse {
		t.Helper()
		rec := x.mustDo(http.MethodGet, consoleUsagePath, nil, nil, http.StatusOK)
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", rec.Header())
		}
		var res consoleUsageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := get()
	if len(res.Buckets) != 2 || res.Total == nil || res.Total.Bytes != 3 {
		t.Fatalf("full key: %+v", res)
	}
	a, b := res.Buckets[0], res.Buckets[1]
	if a.Name != "aaa" || a.Bytes != 3 || a.Objects != 1 || a.MaxSize != 100 || !a.Evict || a.ExpireAfter != "336h0m0s" || a.MeasuredAt.IsZero() {
		t.Errorf("bucket a: %+v", a)
	}
	if !b.Public || !b.Website || !b.CORS || a.Website || a.CORS || b.MaxSize != 0 {
		t.Errorf("bucket b: %+v", b)
	}

	x.as("scoped", "scoped-secret", func() {
		res = get()
	})
	if len(res.Buckets) != 1 || res.Buckets[0].Name != "aaa" || res.Total != nil || res.ReadOnly {
		t.Errorf("scoped key: %+v", res)
	}
	x.as("reader", "reader-secret", func() {
		res = get()
	})
	if !res.ReadOnly || len(res.Buckets) != 2 {
		t.Errorf("read-only key: %+v", res)
	}

	if rec := x.anon(http.MethodGet, consoleUsagePath); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous: %d", rec.Code)
	}
	x.expectErr(http.MethodPost, consoleUsagePath, nil, nil, "MethodNotAllowed")
	x.as("AK", "wrong", func() {
		x.expectErr(http.MethodGet, consoleUsagePath, nil, nil, "SignatureDoesNotMatch")
	})
}
