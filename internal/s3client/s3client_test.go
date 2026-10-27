package s3client

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

// server runs the plainbucket handler for the client to talk to.
func server(t *testing.T, domains ...string) (*Client, *storage.Store) {
	t.Helper()
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.MinPartSize = 5
	t.Cleanup(func() { _ = store.Close() })
	h := s3api.New(s3api.Options{Store: store, Region: "us-east-1", Domains: domains,
		Credentials: []s3api.Credential{{AccessKey: "AK", SecretKey: "SK"}}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c := &Client{Endpoint: u, Signer: sigv4.Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}, HTTP: srv.Client()}
	if len(domains) > 0 {
		// Every bucket host resolves to the test server.
		addr := u.Host
		c.Endpoint = &url.URL{Scheme: "http", Host: domains[0] + ":" + u.Port()}
		c.VirtualHosted = true
		c.HTTP = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	}
	return c, store
}

func TestObjects(t *testing.T) {
	c, store := server(t)
	ctx := context.Background()
	if err := store.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	key := "dir/a b+c=d&e 한글.txt"
	sum := md5.Sum([]byte("hello"))
	meta := Meta{ContentType: "text/plain", UserMeta: map[string]string{"owner": "ci"}, Headers: map[string]string{"Cache-Control": "max-age=60"},
		Tags: []Tag{{"b", "2"}, {"a", "1 &"}}}
	etag, err := c.PutObject(ctx, "bkt", key, strings.NewReader("hello"), 5, sum[:], meta)
	if err != nil || string(MD5(etag)) != string(sum[:]) {
		t.Fatalf("put: %q %v", etag, err)
	}
	o, err := c.HeadObject(ctx, "bkt", key)
	if err != nil || o.Size != 5 || o.ETag != etag || o.Meta.ContentType != "text/plain" || o.Meta.UserMeta["owner"] != "ci" {
		t.Errorf("head: %+v %v", o, err)
	}
	info, _ := store.HeadObject("bkt", key)
	if info.Headers["Cache-Control"] != "max-age=60" || len(info.Tags) != 2 || info.Tags[0] != (storage.Tag{Key: "a", Value: "1 &"}) {
		t.Errorf("stored: %+v", info)
	}

	bad := md5.Sum([]byte("other"))
	if _, err := c.PutObject(ctx, "bkt", "k", strings.NewReader("hello"), 5, bad[:], Meta{}); !errorCode(err, "BadDigest") {
		t.Errorf("wrong md5: %v", err)
	}
	if _, err := c.PutObject(ctx, "bkt", "empty", strings.NewReader(""), 0, nil, Meta{}); err != nil {
		t.Errorf("empty object: %v", err)
	}

	if err := c.PutObjectTagging(ctx, "bkt", key, []Tag{{"z", "26"}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := store.HeadObject("bkt", key); len(info.Tags) != 1 || info.Tags[0].Key != "z" {
		t.Errorf("tags: %+v", info.Tags)
	}
	if tags, err := c.GetObjectTagging(ctx, "bkt", key); err != nil || len(tags) != 1 || tags[0] != (Tag{"z", "26"}) {
		t.Errorf("GetObjectTagging: %v %v", tags, err)
	}
	if err := c.PutObjectTagging(ctx, "bkt", key, nil); err != nil {
		t.Fatal(err)
	}
	if info, _ := store.HeadObject("bkt", key); len(info.Tags) != 0 {
		t.Errorf("tags after removal: %+v", info.Tags)
	}

	if err := c.DeleteObject(ctx, "bkt", key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HeadObject(ctx, "bkt", key); !IsNotFound(err) {
		t.Errorf("head after delete: %v", err)
	}
	if err := c.DeleteObject(ctx, "bkt", key); err != nil {
		t.Errorf("delete of a missing key: %v", err)
	}
	if err := c.DeleteObject(ctx, "nope", key); !errorCode(err, "NoSuchBucket") {
		t.Errorf("delete in a missing bucket: %v", err)
	}
}

func errorCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func TestMultipart(t *testing.T) {
	c, store := server(t)
	ctx := context.Background()
	_ = store.CreateBucket("bkt")
	id, err := c.CreateMultipartUpload(ctx, "bkt", "big", Meta{ContentType: "application/x-test", Tags: []Tag{{"k", "v"}}})
	if err != nil {
		t.Fatal(err)
	}
	var parts []Part
	for i, body := range []string{"hello", "world!"} {
		sum := md5.Sum([]byte(body))
		etag, err := c.UploadPart(ctx, "bkt", "big", id, i+1, strings.NewReader(body), int64(len(body)), sum[:])
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, Part{Number: i + 1, ETag: etag})
	}
	etag, err := c.CompleteMultipartUpload(ctx, "bkt", "big", id, parts)
	if err != nil || !strings.HasSuffix(etag, `-2"`) || MD5(etag) != nil {
		t.Fatalf("complete: %q %v", etag, err)
	}
	if info, _ := store.HeadObject("bkt", "big"); info.Size != 11 || info.ContentType != "application/x-test" || len(info.Tags) != 1 {
		t.Errorf("assembled: %+v", info)
	}

	id, _ = c.CreateMultipartUpload(ctx, "bkt", "dropped", Meta{})
	if err := c.AbortMultipartUpload(ctx, "bkt", "dropped", id); err != nil {
		t.Fatal(err)
	}
	x := md5.Sum([]byte("x"))
	if _, err := c.UploadPart(ctx, "bkt", "dropped", id, 1, strings.NewReader("x"), 1, x[:]); !errorCode(err, "NoSuchUpload") {
		t.Errorf("part of an aborted upload: %v", err)
	}
	if err := c.AbortMultipartUpload(ctx, "bkt", "dropped", id); !errorCode(err, "NoSuchUpload") {
		t.Errorf("abort twice: %v", err)
	}
	if _, err := c.CreateMultipartUpload(ctx, "nope", "k", Meta{}); !errorCode(err, "NoSuchBucket") {
		t.Errorf("create in a missing bucket: %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, "bkt", "big", "no-such-id", parts); !errorCode(err, "NoSuchUpload") {
		t.Errorf("complete of a missing upload: %v", err)
	}
}

func TestListObjectsPages(t *testing.T) {
	c, store := server(t, "s3.test")
	ctx := context.Background()
	_ = store.CreateBucket("bkt")
	want := []string{"odd key%2F.txt", "p/ä", "p/x y"}
	for i := range 1001 {
		want = append(want, fmt.Sprintf("p/n%04d", i))
	}
	for _, k := range want {
		if _, err := store.PutObject("bkt", k, strings.NewReader("x"), 1, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	if err := c.ListObjects(ctx, "bkt", "", func(o Object) error {
		if o.Size != 1 || o.ETag == "" {
			t.Errorf("listed %+v", o)
		}
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got[0] != "odd key%2F.txt" || got[1] != "p/n0000" || got[len(got)-1] != "p/ä" {
		t.Errorf("listed %d keys: first %q, %q, last %q", len(got), got[0], got[1], got[len(got)-1])
	}
	n := 0
	stop := errors.New("stop")
	if err := c.ListObjects(ctx, "bkt", "p/x", func(o Object) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Errorf("prefix and stop: %v after %d", err, n)
	}
	if err := c.ListObjects(ctx, "nope", "", func(Object) error { return nil }); !errorCode(err, "NoSuchBucket") {
		t.Errorf("missing bucket: %v", err)
	}
}

// Some S3 servers report a failed completion inside a 200 response.
func TestErrorDocuments(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{Endpoint: u, Signer: sigv4.Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}, HTTP: srv.Client()}
	ctx := context.Background()

	status, body = http.StatusOK, `<Error><Code>InternalError</Code><Message>try again</Message></Error>`
	if _, err := c.CompleteMultipartUpload(ctx, "b", "k", "id", nil); !errorCode(err, "InternalError") {
		t.Errorf("error in a 200: %v", err)
	}
	status, body = http.StatusOK, `not xml`
	if _, err := c.CompleteMultipartUpload(ctx, "b", "k", "id", nil); err == nil {
		t.Error("garbled completion accepted")
	}
	if _, err := c.CreateMultipartUpload(ctx, "b", "k", Meta{}); err == nil {
		t.Error("garbled create accepted")
	}
	if err := c.ListObjects(ctx, "b", "", func(Object) error { return nil }); err == nil {
		t.Error("garbled listing accepted")
	}
	status, body = http.StatusOK, `<ListBucketResult><EncodingType>url</EncodingType><Contents><Key>%zz</Key></Contents></ListBucketResult>`
	if err := c.ListObjects(ctx, "b", "", func(Object) error { return nil }); err == nil {
		t.Error("undecodable key accepted")
	}
	status, body = http.StatusBadGateway, `<html>proxy</html>`
	if err := c.DeleteObject(ctx, "b", "k"); !errorCode(err, "Bad Gateway") || !strings.Contains(err.Error(), "502") {
		t.Errorf("non-S3 error: %v", err)
	}
}

func TestMD5(t *testing.T) {
	if MD5(`"5d41402abc4b2a76b9719d911017c592"`) == nil || MD5(`"5d41402abc4b2a76b9719d911017c592-2"`) != nil || MD5("zz") != nil {
		t.Error("MD5")
	}
}

func TestStubResponses(t *testing.T) {
	var status int
	var body string
	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rawQuery = r.URL.RawQuery
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{Endpoint: u, Signer: sigv4.Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}, HTTP: srv.Client()}
	ctx := context.Background()

	// S3 encodes a space as '+' in an encoding-type=url listing.
	status, body = http.StatusOK, `<ListBucketResult><EncodingType>url</EncodingType><Contents><Key>a+b%2Bc</Key></Contents></ListBucketResult>`
	var keys []string
	if err := c.ListObjects(ctx, "b", "a b", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil || len(keys) != 1 || keys[0] != "a b+c" {
		t.Errorf("keys %q %v", keys, err)
	}
	if !strings.Contains(rawQuery, "prefix=a%20b") {
		t.Errorf("sent query %q, not the signed encoding", rawQuery)
	}
	status, body = http.StatusOK, `<ListBucketResult><Contents><Key>raw%20key</Key></Contents></ListBucketResult>`
	keys = nil
	if err := c.ListObjects(ctx, "b", "", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil || keys[0] != "raw%20key" {
		t.Errorf("a server ignoring encoding-type: %q %v", keys, err)
	}
	status, body = http.StatusOK, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`
	if err := c.ListObjects(ctx, "b", "", func(Object) error { return nil }); err == nil {
		t.Error("a truncated page without a token accepted")
	}

	status, body = http.StatusOK, `not xml`
	if _, err := c.GetObjectTagging(ctx, "b", "k"); err == nil {
		t.Error("garbled tagging accepted")
	}
	if _, err := c.GetObjectTagging(ctx, "nope", "k"); err == nil {
		t.Error("tagging of a missing object")
	}
	status, body = http.StatusNotFound, `<html>not here</html>`
	if err := c.DeleteObject(ctx, "b", "k"); err == nil {
		t.Error("a proxy's 404 taken as a deleted key")
	}
	status, body = http.StatusOK, `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`
	if _, err := c.CompleteMultipartUpload(ctx, "b", "k", "id", nil); err == nil {
		t.Error("a completion without an ETag accepted")
	}
	if _, err := c.PutObject(ctx, "b", "k", strings.NewReader("x"), 1, nil, Meta{}); err == nil || !strings.Contains(err.Error(), "MD5") {
		t.Errorf("a body over plain http without its MD5: %v", err)
	}
	if _, err := c.PutObject(ctx, "b", "k", strings.NewReader("x"), -1, nil, Meta{}); err == nil {
		t.Error("an unknown size accepted")
	}
}
