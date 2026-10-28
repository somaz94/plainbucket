package s3api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/storage"
)

func TestConditionalDelete(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	etag := x.mustDo(http.MethodPut, "/bkt/k", []byte("hello"), nil, http.StatusOK).Header().Get("ETag")
	when := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(x.root, "bkt", "k"), when, when); err != nil {
		t.Fatal(err)
	}

	for _, hdr := range []map[string]string{
		{"If-Match": `"other"`},
		{"X-Amz-If-Match-Size": "4"},
		{"X-Amz-If-Match-Last-Modified-Time": when.Add(time.Second).Format(http.TimeFormat)},
	} {
		x.expectErr(http.MethodDelete, "/bkt/k", nil, hdr, "PreconditionFailed")
	}
	x.expectErr(http.MethodDelete, "/bkt/k", nil, map[string]string{"X-Amz-If-Match-Size": "-1"}, "InvalidArgument")
	x.expectErr(http.MethodDelete, "/bkt/k", nil, map[string]string{"X-Amz-If-Match-Last-Modified-Time": "yesterday"}, "InvalidArgument")
	x.mustDo(http.MethodHead, "/bkt/k", nil, nil, http.StatusOK)

	x.mustDo(http.MethodDelete, "/bkt/k", nil, map[string]string{
		"If-Match": etag, "X-Amz-If-Match-Size": "5", "X-Amz-If-Match-Last-Modified-Time": when.Format(time.RFC3339),
	}, http.StatusNoContent)
	x.mustDo(http.MethodHead, "/bkt/k", nil, nil, http.StatusNotFound)
	// Deleting what is not there succeeds whatever the condition, as on S3.
	x.mustDo(http.MethodDelete, "/bkt/k", nil, map[string]string{"If-Match": `"other"`}, http.StatusNoContent)
}

func TestConditionalDeleteObjects(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	etag := x.mustDo(http.MethodPut, "/bkt/a", []byte("hello"), nil, http.StatusOK).Header().Get("ETag")
	x.mustDo(http.MethodPut, "/bkt/b", []byte("hello"), nil, http.StatusOK)
	when := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	for _, k := range []string{"a", "b"} {
		if err := os.Chtimes(filepath.Join(x.root, "bkt", k), when, when); err != nil {
			t.Fatal(err)
		}
	}
	body := `<Delete>
<Object><Key>a</Key><ETag>"bad"</ETag></Object>
<Object><Key>b</Key><Size>4</Size></Object>
<Object><Key>missing</Key><ETag>"bad"</ETag></Object>
<Object><Key>b</Key><LastModifiedTime>yesterday</LastModifiedTime></Object>
<Object><Key>b</Key><Size>-1</Size></Object>
</Delete>`
	deleteAll := func(body string) (res deleteResult) {
		t.Helper()
		if err := xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt?delete", []byte(body), nil, http.StatusOK).Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := deleteAll(body)
	if len(res.Errors) != 4 || res.Errors[0].Code != "PreconditionFailed" || res.Errors[1].Code != "PreconditionFailed" ||
		res.Errors[2].Code != "InvalidArgument" || res.Errors[3].Code != "InvalidArgument" ||
		len(res.Deleted) != 1 || res.Deleted[0].Key != "missing" {
		t.Fatalf("failed conditions: %+v", res)
	}
	body = fmt.Sprintf(`<Delete><Quiet>true</Quiet>
<Object><Key>a</Key><ETag>%s</ETag></Object>
<Object><Key>b</Key><Size>5</Size><LastModifiedTime>%s</LastModifiedTime></Object>
</Delete>`, etag, when.Format(http.TimeFormat))
	if res = deleteAll(body); len(res.Errors) != 0 {
		t.Errorf("conditions met: %+v", res)
	}
	x.mustDo(http.MethodHead, "/bkt/a", nil, nil, http.StatusNotFound)
	x.mustDo(http.MethodHead, "/bkt/b", nil, nil, http.StatusNotFound)
}

func TestListUploadsDelimiter(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	for _, k := range []string{"a/1", "a/2", "b", "c/x/1", "d"} {
		x.mustDo(http.MethodPost, "/bkt/"+k+"?uploads", nil, nil, http.StatusOK)
	}
	list := func(q string) (keys []string, res listMultipartUploadsResult) {
		t.Helper()
		if err := xml.Unmarshal(x.mustDo(http.MethodGet, "/bkt?uploads&delimiter=/"+q, nil, nil, http.StatusOK).Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		for _, u := range res.Uploads {
			keys = append(keys, u.Key)
		}
		for _, p := range res.CommonPrefixes {
			keys = append(keys, p.Prefix)
		}
		return keys, res
	}
	if keys, res := list(""); strings.Join(keys, ",") != "b,d,a/,c/" || res.IsTruncated || res.Delimiter != "/" {
		t.Errorf("all: %v %+v", keys, res)
	}
	keys, res := list("&max-uploads=2")
	if strings.Join(keys, ",") != "b,a/" || !res.IsTruncated || res.NextKeyMarker != "b" {
		t.Fatalf("page 1: %v %+v", keys, res)
	}
	keys, res = list("&max-uploads=2&key-marker=" + res.NextKeyMarker + "&upload-id-marker=" + res.NextUploadIDMarker)
	if strings.Join(keys, ",") != "d,c/" || res.IsTruncated {
		t.Fatalf("page 2: %v %+v", keys, res)
	}
	// A page ending in a common prefix resumes after everything under it.
	if keys, res = list("&max-uploads=1"); strings.Join(keys, ",") != "a/" || res.NextKeyMarker != "a/" || res.NextUploadIDMarker != "" {
		t.Errorf("page ending in a/: %v %+v", keys, res)
	}
	if keys, res = list("&key-marker=a/"); strings.Join(keys, ",") != "b,d,c/" || res.IsTruncated {
		t.Errorf("after a/: %v %+v", keys, res)
	}
	if keys, _ = list("&prefix=c/"); strings.Join(keys, ",") != "c/x/" {
		t.Errorf("prefix c/: %v", keys)
	}
	if keys, _ = list("&prefix=c/&key-marker=c/x/"); len(keys) != 0 {
		t.Errorf("prefix c/ after c/x/: %v", keys)
	}
	// A marker inside a common prefix lists that prefix again for the keys after it.
	if keys, _ = list("&key-marker=a/1"); strings.Join(keys, ",") != "b,d,a/,c/" {
		t.Errorf("after a/1: %v", keys)
	}
	if keys, res = list("&max-uploads=0"); len(keys) != 0 || res.IsTruncated {
		t.Errorf("max-uploads=0: %v %+v", keys, res)
	}

	// Two uploads of one key split by a page break resume by upload ID.
	x.mustDo(http.MethodPost, "/bkt/b?uploads", nil, nil, http.StatusOK)
	keys, res = list("&max-uploads=2")
	if strings.Join(keys, ",") != "b,a/" || res.NextKeyMarker != "b" || res.NextUploadIDMarker == "" {
		t.Fatalf("page 1 of two b uploads: %v %+v", keys, res)
	}
	if keys, _ = list("&key-marker=b&upload-id-marker=" + res.NextUploadIDMarker); strings.Join(keys, ",") != "b,d,c/" {
		t.Errorf("page 2 of two b uploads: %v", keys)
	}

	// encoding-type=url covers the prefixes and markers too.
	x.mustDo(http.MethodPost, "/bkt/e%20f/1?uploads", nil, nil, http.StatusOK)
	keys, res = list("&encoding-type=url&prefix=e%20&max-uploads=1")
	if strings.Join(keys, ",") != "e%20f/" || res.Prefix != "e%20" || res.NextKeyMarker != "" {
		t.Errorf("encoded: %v %+v", keys, res)
	}
}

// DeleteObjects flushes each directory it changed once, not once per key.
func TestDeleteObjectsFlushesOncePerDirectory(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	var body strings.Builder
	body.WriteString("<Delete>")
	for i := range 5 {
		k := fmt.Sprintf("k%d", i)
		x.mustDo(http.MethodPut, "/bkt/"+k, []byte("v"), nil, http.StatusOK)
		body.WriteString("<Object><Key>" + k + "</Key></Object>")
	}
	body.WriteString("</Delete>")
	var mu sync.Mutex
	flushed := map[string]int{}
	t.Cleanup(storage.SetSyncDirForTest(func(dir string) error {
		mu.Lock()
		defer mu.Unlock()
		flushed[filepath.ToSlash(dir)]++
		return nil
	}))
	x.mustDo(http.MethodPost, "/bkt?delete", []byte(body.String()), nil, http.StatusOK)
	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 || flushed["bkt"] != 1 {
		t.Errorf("flushed %v, want bkt once", flushed)
	}
}
