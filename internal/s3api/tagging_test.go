package s3api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func tagDoc(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString("<Tagging><TagSet>")
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", pairs[i], pairs[i+1])
	}
	b.WriteString("</TagSet></Tagging>")
	return []byte(b.String())
}

// tagsOf returns an object's tags as k=v pairs in order.
func (x *harness) tagsOf(target string) string {
	x.t.Helper()
	var doc tagging
	if err := xml.Unmarshal(x.mustDo(http.MethodGet, target+"?tagging", nil, nil, http.StatusOK).Body.Bytes(), &doc); err != nil {
		x.t.Fatal(err)
	}
	var out []string
	for _, t := range doc.TagSet.Tags {
		out = append(out, t.Key+"="+t.Value)
	}
	return strings.Join(out, "&")
}

func TestObjectTagging(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.PublicBuckets = []string{"bkt"}
		o.Credentials = append(o.Credentials, Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true},
			Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"other"}})
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("x"), map[string]string{"X-Amz-Tagging": "b=2&a=1&sp%20ace=v%2Bv&empty="}, http.StatusOK)
	if got := x.tagsOf("/bkt/k"); got != "a=1&b=2&empty=&sp ace=v+v" {
		t.Errorf("tags from the header: %s", got)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if n := x.mustDo(m, "/bkt/k", nil, nil, http.StatusOK).Header().Get("X-Amz-Tagging-Count"); n != "4" {
			t.Errorf("%s tag count = %q", m, n)
		}
		if rec := x.anon(m, "/bkt/k"); rec.Code != http.StatusOK || rec.Header().Get("X-Amz-Tagging-Count") != "" {
			t.Errorf("anonymous %s: %d, count %q", m, rec.Code, rec.Header().Get("X-Amz-Tagging-Count"))
		}
	}

	x.mustDo(http.MethodPut, "/bkt/k?tagging", tagDoc("z", "26", "y", ""), nil, http.StatusOK)
	if got := x.tagsOf("/bkt/k"); got != "y=&z=26" {
		t.Errorf("put tagging: %s", got)
	}
	x.mustDo(http.MethodDelete, "/bkt/k?tagging", nil, nil, http.StatusNoContent)
	if got := x.tagsOf("/bkt/k"); got != "" {
		t.Errorf("after delete: %s", got)
	}
	if h := x.mustDo(http.MethodHead, "/bkt/k", nil, nil, http.StatusOK).Header(); h.Get("X-Amz-Tagging-Count") != "" {
		t.Error("tag count without tags")
	}

	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprint(i), strings.Repeat("v", 256))
	}
	x.mustDo(http.MethodPut, "/bkt/k?tagging", tagDoc(ten...), nil, http.StatusOK)
	for name, doc := range map[string][]byte{
		"eleven":    tagDoc(append(ten, "10", "v")...),
		"long key":  tagDoc(strings.Repeat("k", 129), "v"),
		"long val":  tagDoc("k", strings.Repeat("v", 257)),
		"empty key": tagDoc("", "v"),
		"duplicate": tagDoc("k", "1", "k", "2"),
		"aws:":      tagDoc("aws:x", "v"),
	} {
		x.t.Run(name, func(t *testing.T) { x.expectErr(http.MethodPut, "/bkt/k?tagging", doc, nil, "InvalidTag") })
	}
	if got := x.tagsOf("/bkt/k"); strings.Count(got, "=") != 10 {
		t.Errorf("a refused set replaced the tags: %s", got)
	}
	// Characters, not bytes, count.
	x.mustDo(http.MethodPut, "/bkt/k?tagging", tagDoc(strings.Repeat("가", 128), strings.Repeat("나", 256)), nil, http.StatusOK)
	x.expectErr(http.MethodPut, "/bkt/k?tagging", []byte("<Tagging><TagSet>"), nil, "MalformedXML")
	x.expectErr(http.MethodPut, "/bkt/k?tagging", []byte("<Tagging/>"), nil, "MalformedXML")
	x.expectErr(http.MethodPut, "/bkt/k2", []byte("x"), map[string]string{"X-Amz-Tagging": "a=%zz"}, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/k2", []byte("x"), map[string]string{"X-Amz-Tagging": "a=1&a=2"}, "InvalidTag")
	x.expectErr(http.MethodPut, "/bkt/missing?tagging", tagDoc("a", "1"), nil, "NoSuchKey")
	x.expectErr(http.MethodDelete, "/bkt/missing?tagging", nil, nil, "NoSuchKey")

	// Tags are a write for read-only keys and never anonymous, even in a public bucket.
	x.as("reader", "reader-secret", func() {
		x.mustDo(http.MethodGet, "/bkt/k?tagging", nil, nil, http.StatusOK)
		x.expectErr(http.MethodPut, "/bkt/k?tagging", tagDoc("a", "1"), nil, "AccessDenied")
		x.expectErr(http.MethodDelete, "/bkt/k?tagging", nil, nil, "AccessDenied")
	})
	x.as("scoped", "scoped-secret", func() {
		x.expectErr(http.MethodGet, "/bkt/k?tagging", nil, nil, "AccessDenied")
		x.expectErr(http.MethodPut, "/bkt/k?tagging", tagDoc("a", "1"), nil, "AccessDenied")
	})
	x.expectErr(http.MethodDelete, "/bkt?tagging", nil, nil, "NotImplemented")
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := x.anon(m, "/bkt/k?tagging"); rec.Code != http.StatusForbidden {
			t.Errorf("anonymous %s: %d", m, rec.Code)
		}
	}
}

func TestTaggingOnCopyAndMultipart(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/src", []byte("x"), map[string]string{"X-Amz-Tagging": "a=1"}, http.StatusOK)

	x.mustDo(http.MethodPut, "/bkt/copy", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Tagging": "ignored=1"}, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/meta", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Metadata-Directive": "REPLACE"}, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/new", nil, map[string]string{
		"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Tagging-Directive": "REPLACE", "X-Amz-Tagging": "b=2",
	}, http.StatusOK)
	for target, want := range map[string]string{"/bkt/copy": "a=1", "/bkt/meta": "a=1", "/bkt/new": "b=2"} {
		if got := x.tagsOf(target); got != want {
			t.Errorf("%s: %s, want %s", target, got, want)
		}
	}
	x.expectErr(http.MethodPut, "/bkt/bad", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Tagging-Directive": "MERGE"}, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/bad", nil, map[string]string{
		"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Tagging-Directive": "REPLACE", "X-Amz-Tagging": "aws:x=1",
	}, "InvalidTag")

	var up initiateMultipartUploadResult
	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/mp?uploads", nil, map[string]string{"X-Amz-Tagging": "Hello=World&foo=bar"}, http.StatusOK).Body.Bytes(), &up)
	etag := x.mustDo(http.MethodPut, "/bkt/mp?partNumber=1&uploadId="+up.UploadID, []byte("x"), nil, http.StatusOK).Header().Get("ETag")
	x.mustDo(http.MethodPost, "/bkt/mp?uploadId="+up.UploadID,
		[]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+etag+`</ETag></Part></CompleteMultipartUpload>`), nil, http.StatusOK)
	if got := x.tagsOf("/bkt/mp"); got != "Hello=World&foo=bar" {
		t.Errorf("multipart: %s", got)
	}
	x.expectErr(http.MethodPost, "/bkt/mp2?uploads", nil, map[string]string{"X-Amz-Tagging": "aws:x=1"}, "InvalidTag")
}

func TestPostObjectTagging(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	exp := testNow.Add(time.Hour)
	doc := string(tagDoc("a", "1"))
	conds := []any{map[string]string{"bucket": "bkt"}, map[string]string{"key": "k"}, map[string]string{"tagging": doc}}
	fields := signedForm(x.signer, exp, conds, [2]string{"key", "k"}, [2]string{"tagging", doc})
	if rec := x.post("/bkt", postForm{fields: fields, file: "x"}); rec.Code != http.StatusNoContent {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	if got := x.tagsOf("/bkt/k"); got != "a=1" {
		t.Errorf("post tags: %s", got)
	}
	bad := string(tagDoc("aws:x", "1"))
	conds[2] = map[string]string{"tagging": bad}
	fields = signedForm(x.signer, exp, conds, [2]string{"key", "k"}, [2]string{"tagging", bad})
	if rec := x.post("/bkt", postForm{fields: fields, file: "x"}); errCode(rec) != "InvalidTag" {
		t.Errorf("bad post tags: %d %s", rec.Code, rec.Body.String())
	}
}
