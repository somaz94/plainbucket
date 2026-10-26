package s3api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestConsoleTrash(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Console = true
		o.Credentials = append(o.Credentials,
			Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true},
			Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"other"}})
	})
	x.h.store.SetTrash(map[string]time.Duration{"bkt": 24 * time.Hour})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("hello"), map[string]string{"Content-Type": "text/x"}, http.StatusOK)
	x.mustDo(http.MethodDelete, "/bkt/k", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodHead, "/bkt/k", nil, nil, http.StatusNotFound)

	list := func() consoleTrashResponse {
		t.Helper()
		var res consoleTrashResponse
		rec := x.mustDo(http.MethodGet, consoleTrashPath+"?bucket=bkt", nil, nil, http.StatusOK)
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", rec.Header())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := list()
	if res.TrashAfter != "24h0m0s" || len(res.Entries) != 1 || res.Entries[0].Key != "k" || res.Entries[0].Size != 5 || res.Bytes != 5 {
		t.Fatalf("list: %+v", res)
	}
	id := res.Entries[0].ID

	// The key's bucket scope and read-only flag apply; listing is a read.
	x.as("reader", "reader-secret", func() {
		x.mustDo(http.MethodGet, consoleTrashPath+"?bucket=bkt", nil, nil, http.StatusOK)
		x.expectErr(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=restore&id="+id, nil, nil, "AccessDenied")
	})
	x.as("scoped", "scoped-secret", func() {
		x.expectErr(http.MethodGet, consoleTrashPath+"?bucket=bkt", nil, nil, "AccessDenied")
	})
	if rec := x.anon(http.MethodGet, consoleTrashPath+"?bucket=bkt"); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous: %d", rec.Code)
	}
	x.expectErr(http.MethodPut, consoleTrashPath+"?bucket=bkt", nil, nil, "MethodNotAllowed")
	x.expectErr(http.MethodGet, consoleTrashPath+"?bucket=missing", nil, nil, "NoSuchBucket")
	x.expectErr(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=undelete&id="+id, nil, nil, "InvalidArgument")
	x.expectErr(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=restore&id=nope", nil, nil, "NoSuchKey")

	var restored map[string]string
	rec := x.mustDo(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=restore&id="+id, nil, nil, http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &restored); err != nil || restored["key"] != "k" || restored["etag"] == "" {
		t.Errorf("restore: %s", rec.Body.String())
	}
	if got := x.mustDo(http.MethodGet, "/bkt/k", nil, nil, http.StatusOK); got.Body.String() != "hello" || got.Header().Get("Content-Type") != "text/x" {
		t.Errorf("restored object: %v %q", got.Header(), got.Body.String())
	}

	x.mustDo(http.MethodDelete, "/bkt/k", nil, nil, http.StatusNoContent)
	id = list().Entries[0].ID
	x.mustDo(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=purge&id="+id, nil, nil, http.StatusNoContent)
	if res := list(); len(res.Entries) != 0 {
		t.Errorf("after purge: %+v", res)
	}

	// DeleteObjects trashes too.
	x.mustDo(http.MethodPut, "/bkt/a", []byte("1"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/b", []byte("22"), nil, http.StatusOK)
	x.mustDo(http.MethodPost, "/bkt?delete", []byte(`<Delete><Object><Key>a</Key></Object><Object><Key>b</Key></Object></Delete>`), nil, http.StatusOK)
	if res := list(); len(res.Entries) != 2 || res.Bytes != 3 {
		t.Errorf("after DeleteObjects: %+v", res)
	}

	var usage consoleUsageResponse
	_ = json.Unmarshal(x.mustDo(http.MethodGet, consoleUsagePath, nil, nil, http.StatusOK).Body.Bytes(), &usage)
	if len(usage.Buckets) != 1 || usage.Buckets[0].TrashAfter != "24h0m0s" {
		t.Errorf("usage: %+v", usage)
	}
}
