package s3api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyMode(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Console = true
		o.Metrics = true
		o.Credentials = append(o.Credentials, Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true})
		o.Buckets = map[string]BucketPolicy{"bkt": {Public: true, CORS: &CORS{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET", "PUT"}}}}
	})
	x.h.store.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("data"), nil, http.StatusOK)
	x.mustDo(http.MethodDelete, "/bkt/k", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("data"), nil, http.StatusOK)

	x.h.SetReadOnly(true)
	for _, c := range []struct {
		method, target string
		body           []byte
		hdr            map[string]string
	}{
		{http.MethodPut, "/bkt/new", []byte("x"), nil},
		{http.MethodPut, "/bkt/copy", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/k"}},
		{http.MethodPut, "/bkt/k?tagging", []byte("<Tagging><TagSet/></Tagging>"), nil},
		{http.MethodPut, "/bkt/k?partNumber=1&uploadId=x", []byte("x"), nil},
		{http.MethodDelete, "/bkt", nil, nil},
		{http.MethodPost, consoleTrashPath + "?bucket=bkt&action=restore&id=x", nil, nil},
		{http.MethodDelete, "/bkt/k", nil, nil},
		{http.MethodPost, "/bkt/k?uploads", nil, nil},
		{http.MethodPost, "/bkt?delete", []byte("<Delete><Object><Key>k</Key></Object></Delete>"), nil},
		{http.MethodPut, "/other", nil, nil},
		{http.MethodPost, consoleTrashPath + "?bucket=bkt&action=purge&id=x", nil, nil},
	} {
		rec := x.do(c.method, c.target, c.body, c.hdr)
		if rec.Code != http.StatusServiceUnavailable || errCode(rec) != "ServiceUnavailable" || rec.Header().Get("Retry-After") != readOnlyRetry {
			t.Errorf("%s %s = %d %s %v", c.method, c.target, rec.Code, errCode(rec), rec.Header())
		}
	}
	exp := testNow.Add(time.Hour)
	conds := []any{map[string]string{"bucket": "bkt"}, []string{"starts-with", "$key", ""}}
	if rec := x.post("/bkt", postForm{fields: signedForm(x.signer, exp, conds, [2]string{"key", "p"}), filename: "f", file: "x"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("post upload: %d %s", rec.Code, rec.Body.String())
	}
	// Reads go on, and the key's own read-only flag still answers 403.
	if got := x.mustDo(http.MethodGet, "/bkt/k", nil, nil, http.StatusOK).Body.String(); got != "data" {
		t.Errorf("read: %q", got)
	}
	x.mustDo(http.MethodGet, "/bkt?list-type=2", nil, nil, http.StatusOK)
	x.mustDo(http.MethodGet, consoleTrashPath+"?bucket=bkt", nil, nil, http.StatusOK)
	x.as("reader", "reader-secret", func() { x.expectErr(http.MethodPut, "/bkt/new", []byte("x"), nil, "AccessDenied") })
	if rec := x.anon(http.MethodGet, "/bkt/k"); rec.Code != http.StatusOK {
		t.Errorf("anonymous read: %d", rec.Code)
	}
	// A browser's preflight for an upload is answered; only the upload itself is refused.
	pre := httptest.NewRequest(http.MethodOptions, "http://pb.test/bkt/k", nil)
	pre.Header.Set("Origin", "https://app.example.com")
	pre.Header.Set("Access-Control-Request-Method", "PUT")
	preRec := httptest.NewRecorder()
	x.h.ServeHTTP(preRec, pre)
	if preRec.Code != http.StatusOK {
		t.Errorf("preflight: %d", preRec.Code)
	}

	var usage consoleUsageResponse
	_ = json.Unmarshal(x.mustDo(http.MethodGet, consoleUsagePath, nil, nil, http.StatusOK).Body.Bytes(), &usage)
	if !usage.ReadOnly || !usage.Maintenance {
		t.Errorf("usage: %+v", usage)
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{"plainbucket_read_only 1", "plainbucket_writes_in_flight 0"} {
		if !strings.Contains(rec.Body.String(), line) {
			t.Errorf("metrics lack %q", line)
		}
	}

	x.h.SetReadOnly(false)
	x.mustDo(http.MethodPut, "/bkt/new", []byte("x"), nil, http.StatusOK)
	var after consoleUsageResponse
	_ = json.Unmarshal(x.mustDo(http.MethodGet, consoleUsagePath, nil, nil, http.StatusOK).Body.Bytes(), &after)
	if after.ReadOnly || after.Maintenance {
		t.Errorf("usage after: %+v", after)
	}
}

func TestReadOnlyOption(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.ReadOnly = true })
	x.expectErr(http.MethodPut, "/bkt", nil, nil, "ServiceUnavailable")
	x.h.SetReadOnly(false)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
}
