package s3api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/ratelimit"
)

func TestKeyRateLimit(t *testing.T) {
	limited := Credential{AccessKey: "slow", SecretKey: "slow-secret", RateLimit: ratelimit.Limit{Requests: 0.001, Burst: 2}}
	x := newHarness(t, func(o *Options) {
		o.Console = true
		o.Metrics = true
		o.Credentials = append(o.Credentials, limited)
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.as("slow", "slow-secret", func() {
		x.mustDo(http.MethodPut, "/bkt/a", []byte("1"), nil, http.StatusOK)
		x.mustDo(http.MethodGet, "/bkt/a", nil, nil, http.StatusOK)
		rec := x.do(http.MethodGet, "/bkt/a", nil, nil)
		if rec.Code != http.StatusServiceUnavailable || errCode(rec) != "SlowDown" || rec.Header().Get("Retry-After") == "" {
			t.Errorf("past the burst: %d %s %v", rec.Code, errCode(rec), rec.Header())
		}
		// The console API counts against the same key.
		x.expectErr(http.MethodGet, consoleUsagePath, nil, nil, "SlowDown")
	})
	// Another key is not held back.
	for range 5 {
		x.mustDo(http.MethodGet, "/bkt/a", nil, nil, http.StatusOK)
	}

	// A reload with the same limit keeps the spent allowance; a changed one starts afresh.
	keep := []Credential{{AccessKey: "AK", SecretKey: "SK"}, limited}
	x.h.SetAccess(keep, nil, nil)
	x.as("slow", "slow-secret", func() { x.expectErr(http.MethodGet, "/bkt/a", nil, nil, "SlowDown") })
	changed := limited
	changed.RateLimit.Burst = 3
	x.h.SetAccess([]Credential{{AccessKey: "AK", SecretKey: "SK"}, changed}, nil, nil)
	x.as("slow", "slow-secret", func() { x.mustDo(http.MethodGet, "/bkt/a", nil, nil, http.StatusOK) })

	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `plainbucket_throttled_requests_total{client="key"} 3`) {
		t.Errorf("throttle metric:\n%s", rec.Body.String())
	}
}

func TestPostUploadRateLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = []Credential{{AccessKey: "AK", SecretKey: "SK", RateLimit: ratelimit.Limit{Requests: 0.001, Burst: 1}}}
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	conds := []any{map[string]string{"bucket": "bkt"}, []string{"starts-with", "$key", ""}}
	rec := x.post("/bkt", postForm{fields: signedForm(x.signer, testNow.Add(time.Hour), conds, [2]string{"key", "p"}), filename: "f", file: "x"})
	if rec.Code != http.StatusServiceUnavailable || errCode(rec) != "SlowDown" {
		t.Errorf("post past the burst: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnonymousRateLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Buckets = map[string]BucketPolicy{"pub": {Public: true}}
		o.AnonymousRateLimit = ratelimit.Limit{Requests: 0.001, Burst: 1}
	})
	x.mustDo(http.MethodPut, "/pub", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/pub/k", []byte("x"), nil, http.StatusOK)
	from := func(addr string) int {
		r := httptest.NewRequest(http.MethodGet, "/pub/k", nil)
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec.Code
	}
	if from("198.51.100.1:1000") != http.StatusOK || from("198.51.100.1:2000") != http.StatusServiceUnavailable {
		t.Error("one address was not limited")
	}
	if from("198.51.100.2:1000") != http.StatusOK {
		t.Error("another address shares the first one's limit")
	}
	// Signed requests are not anonymous ones.
	x.mustDo(http.MethodGet, "/pub/k", nil, nil, http.StatusOK)

	x.h.SetAnonymousRateLimit(ratelimit.Limit{})
	if from("198.51.100.1:3000") != http.StatusOK {
		t.Error("limit still applied after turning it off")
	}
}

func TestKeyBandwidthLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = []Credential{{AccessKey: "AK", SecretKey: "SK", RateLimit: ratelimit.Limit{Bytes: 100 << 10}}}
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	data := bytes.Repeat([]byte("x"), 20<<10)
	start := time.Now()
	x.mustDo(http.MethodPut, "/bkt/k", data, nil, http.StatusOK)
	if got := x.mustDo(http.MethodGet, "/bkt/k", nil, nil, http.StatusOK).Body.Len(); got != len(data) {
		t.Fatalf("read %d bytes", got)
	}
	// 20 KiB each way at 100 KiB/s.
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Errorf("40 KiB took %v", d)
	}
}

func TestPostUploadBandwidthLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = []Credential{{AccessKey: "AK", SecretKey: "SK", RateLimit: ratelimit.Limit{Bytes: 100 << 10}}}
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	conds := []any{map[string]string{"bucket": "bkt"}, []string{"starts-with", "$key", ""}}
	start := time.Now()
	file := strings.Repeat("x", 20<<10)
	if rec := x.post("/bkt", postForm{fields: signedForm(x.signer, testNow.Add(time.Hour), conds, [2]string{"key", "p"}), filename: "f", file: file}); rec.Code != http.StatusNoContent {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	// The file part is read through the limiter too, not only the form around it.
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("a 20 KiB POST at 100 KiB/s took %v", d)
	}
}

func TestClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7:1234":          "198.51.100.7",
		"[2001:db8:1:2:aaaa::1]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb::9]:443": "2001:db8:1:2::/64",
		"[::ffff:198.51.100.7]:80":   "198.51.100.7",
		"[fe80::1%eth0]:80":          "fe80::/64",
		"not an address":             "not an address",
	} {
		if got := clientKey(in); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}
