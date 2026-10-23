package s3api

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
	"github.com/somaz94/plainbucket/internal/storage"
)

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	h      *Handler
	root   string
	signer sigv4test.Signer
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	root := t.TempDir()
	store, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	store.MinPartSize = 5
	logs := &bytes.Buffer{}
	opts := Options{
		Store: store, Credentials: []Credential{{AccessKey: "AK", SecretKey: "SK"}}, Region: "us-east-1",
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&opts)
	}
	h := New(opts)
	h.now = func() time.Time { return testNow }
	return &harness{t: t, h: h, root: root, signer: sigv4test.Signer{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}, logs: logs}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// do signs and serves a request. A header X-Amz-Content-Sha256 overrides the payload hash.
func (x *harness) do(method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	x.t.Helper()
	r := httptest.NewRequest(method, "http://pb.test"+target, bytes.NewReader(body))
	payload := sha(body)
	for k, v := range hdr {
		if strings.EqualFold(k, "X-Amz-Content-Sha256") {
			payload = v
			continue
		}
		r.Header.Set(k, v)
	}
	x.signer.Sign(r, payload, testNow)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func (x *harness) mustDo(method, target string, body []byte, hdr map[string]string, want int) *httptest.ResponseRecorder {
	x.t.Helper()
	rec := x.do(method, target, body, hdr)
	if rec.Code != want {
		x.t.Fatalf("%s %s = %d, want %d: %s", method, target, rec.Code, want, rec.Body.String())
	}
	return rec
}

func errCode(rec *httptest.ResponseRecorder) string {
	var e struct{ Code string }
	_ = xml.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func (x *harness) expectErr(method, target string, body []byte, hdr map[string]string, code string) {
	x.t.Helper()
	rec := x.do(method, target, body, hdr)
	if got := errCode(rec); got != code {
		x.t.Errorf("%s %s: code %q (%d), want %s: %s", method, target, got, rec.Code, code, rec.Body.String())
	}
}

func TestHealthzAndAnonymous(t *testing.T) {
	x := newHarness(t, nil)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bkt/key", nil))
	if rec.Code != http.StatusForbidden || errCode(rec) != "AccessDenied" || rec.Header().Get("X-Amz-Request-Id") == "" {
		t.Errorf("anonymous: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(x.logs.String(), "request denied") {
		t.Error("denial not logged at debug")
	}
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/bkt/key", nil))
	if rec.Code != http.StatusForbidden || rec.Body.Len() != 0 {
		t.Errorf("anonymous HEAD: %d %q", rec.Code, rec.Body.String())
	}
}

func TestBucketOps(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.Region = "eu-west-1" })
	x.expectErr(http.MethodPut, "/", nil, nil, "MethodNotAllowed")
	cfg := []byte(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`)
	rec := x.mustDo(http.MethodPut, "/bkt", cfg, nil, http.StatusOK)
	if rec.Header().Get("Location") != "/bkt" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	x.expectErr(http.MethodPut, "/bad", []byte("<nope"), nil, "MalformedXML")
	x.expectErr(http.MethodPut, "/bkt", nil, nil, "BucketAlreadyOwnedByYou")
	x.expectErr(http.MethodPut, "/Bad_Name", nil, nil, "InvalidBucketName")

	rec = x.mustDo(http.MethodHead, "/bkt", nil, nil, http.StatusOK)
	if rec.Header().Get("X-Amz-Bucket-Region") != "eu-west-1" {
		t.Errorf("region header = %q", rec.Header().Get("X-Amz-Bucket-Region"))
	}
	x.mustDo(http.MethodHead, "/missing", nil, nil, http.StatusNotFound)
	rec = x.mustDo(http.MethodGet, "/bkt?location", nil, nil, http.StatusOK)
	if !strings.Contains(rec.Body.String(), ">eu-west-1</LocationConstraint>") {
		t.Errorf("location: %s", rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/missing?location", nil, nil, "NoSuchBucket")
	rec = x.mustDo(http.MethodGet, "/bkt?versioning", nil, nil, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "VersioningConfiguration") {
		t.Errorf("versioning: %s", rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/missing?versioning", nil, nil, "NoSuchBucket")

	rec = x.mustDo(http.MethodGet, "/", nil, nil, http.StatusOK)
	var lb listAllMyBucketsResult
	if xml.Unmarshal(rec.Body.Bytes(), &lb) != nil || len(lb.Buckets) != 1 || lb.Buckets[0].Name != "bkt" {
		t.Errorf("list buckets: %s", rec.Body.String())
	}

	x.mustDo(http.MethodPut, "/bkt/k", []byte("x"), nil, http.StatusOK)
	x.expectErr(http.MethodDelete, "/bkt", nil, nil, "BucketNotEmpty")
	x.expectErr(http.MethodPost, "/bkt", nil, nil, "MethodNotAllowed")
	x.expectErr(http.MethodPatch, "/bkt", nil, nil, "MethodNotAllowed")
	x.expectErr(http.MethodPatch, "/bkt/k", nil, nil, "MethodNotAllowed")
	x.mustDo(http.MethodDelete, "/bkt/k", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodDelete, "/bkt", nil, nil, http.StatusNoContent)
	x.expectErr(http.MethodDelete, "/bkt", nil, nil, "NoSuchBucket")
}

func TestUnsupportedRequests(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("x"), nil, http.StatusOK)
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/bkt?acl"},
		{http.MethodPut, "/bkt/k?tagging"},
		{http.MethodDelete, "/bkt/k?tagging"},
		{http.MethodGet, "/bkt?tagging"},
		{http.MethodGet, "/bkt/k?versionId=abc"},
		{http.MethodPut, "/bkt/k?attributes"},
		{http.MethodGet, "/bkt?attributes"},
		{http.MethodPut, "/bkt?versioning"},
		{http.MethodGet, "/bkt/k?versioning"},
		{http.MethodGet, "/bkt/k?versions"},
		{http.MethodPut, "/bkt?versions"},
	} {
		x.expectErr(c.method, c.target, nil, nil, "NotImplemented")
	}
	x.mustDo(http.MethodGet, "/bkt/k?versionId=null", nil, nil, http.StatusOK)
	x.expectErr(http.MethodGet, "/bkt?list-type=1", nil, nil, "InvalidArgument")
	etag := x.mustDo(http.MethodPut, "/bkt/k", []byte("x"), map[string]string{"If-None-Match": `"other"`}, http.StatusOK).Header().Get("ETag")
	x.expectErr(http.MethodPut, "/bkt/k", []byte("y"), map[string]string{"If-None-Match": etag}, "PreconditionFailed")
}

func TestPutGetHead(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	body := []byte("hello world")
	md5sum := md5.Sum(body)
	rec := x.mustDo(http.MethodPut, "/bkt/dir/hello.txt", body, map[string]string{
		"Content-Type":        "text/plain",
		"Content-MD5":         base64.StdEncoding.EncodeToString(md5sum[:]),
		"Cache-Control":       "max-age=1",
		"Content-Disposition": "inline",
		"X-Amz-Meta-Color":    "blue",
	}, http.StatusOK)
	etag := `"` + hex.EncodeToString(md5sum[:]) + `"`
	if rec.Header().Get("ETag") != etag || rec.Header().Get("X-Amz-Checksum-Crc64nvme") == "" {
		t.Errorf("put headers: %v", rec.Header())
	}
	if raw, _ := os.ReadFile(filepath.Join(x.root, "bkt", "dir", "hello.txt")); string(raw) != "hello world" {
		t.Errorf("on disk %q", raw)
	}

	rec = x.mustDo(http.MethodGet, "/bkt/dir/hello.txt?response-content-type=application/x-override", nil, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}, http.StatusOK)
	h := rec.Header()
	if rec.Body.String() != "hello world" || h.Get("ETag") != etag || h.Get("Content-Type") != "application/x-override" ||
		h.Get("Cache-Control") != "max-age=1" || h.Get("Content-Disposition") != "inline" || rawHeader(h, "x-amz-meta-color") != "blue" ||
		h.Get("X-Amz-Checksum-Crc64nvme") == "" || h.Get("X-Amz-Checksum-Type") != "FULL_OBJECT" {
		t.Errorf("get: %q %v", rec.Body.String(), h)
	}
	rec = x.mustDo(http.MethodGet, "/bkt/dir/hello.txt", nil, map[string]string{"Range": "bytes=0-4", "X-Amz-Checksum-Mode": "ENABLED"}, http.StatusPartialContent)
	if rec.Body.String() != "hello" || rec.Header().Get("X-Amz-Checksum-Crc64nvme") != "" {
		t.Errorf("range: %q %v", rec.Body.String(), rec.Header())
	}
	x.mustDo(http.MethodGet, "/bkt/dir/hello.txt", nil, map[string]string{"If-None-Match": etag}, http.StatusNotModified)
	x.mustDo(http.MethodGet, "/bkt/dir/hello.txt", nil, map[string]string{"If-Match": `"other"`}, http.StatusPreconditionFailed)
	rec = x.mustDo(http.MethodHead, "/bkt/dir/hello.txt", nil, nil, http.StatusOK)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "11" {
		t.Errorf("head: %q %v", rec.Body.String(), rec.Header())
	}
	x.mustDo(http.MethodHead, "/bkt/missing", nil, nil, http.StatusNotFound)
	x.expectErr(http.MethodGet, "/bkt/missing", nil, nil, "NoSuchKey")
	x.expectErr(http.MethodGet, "/nobkt/missing", nil, nil, "NoSuchBucket")

	x.mustDo(http.MethodPut, "/bkt/dir/sub/", nil, nil, http.StatusOK)
	rec = x.mustDo(http.MethodGet, "/bkt/dir/sub/", nil, nil, http.StatusOK)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != storage.DirContentType {
		t.Errorf("marker: %v", rec.Header())
	}
	x.expectErr(http.MethodPut, "/bkt/dir/hello.txt/x", []byte("x"), nil, "KeyConflict")
	x.expectErr(http.MethodPut, "/bkt/a//b", []byte("x"), nil, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/m/", []byte("x"), nil, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/"+strings.Repeat("k", 300), []byte("x"), nil, "KeyTooLongError")

	x.mustDo(http.MethodPut, "/bkt/plain", []byte("x"), map[string]string{"X-Amz-Content-Sha256": sigv4.UnsignedPayload}, http.StatusOK)
	rec = x.mustDo(http.MethodGet, "/bkt/plain", nil, nil, http.StatusOK)
	if rec.Header().Get("Content-Type") != defaultContentType {
		t.Errorf("default content type = %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(x.logs.String(), "msg=request") {
		t.Error("request logged without the access log")
	}
}

func TestAccessLog(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.AccessLog = true })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/dir/k", []byte("x"), nil, http.StatusOK)
	for _, want := range []string{"level=INFO msg=request", "operation=PutObject", "method=PUT", "bucket=bkt", "key=dir/k", "status=200", "access_key=AK", "request_id="} {
		if !strings.Contains(x.logs.String(), want) {
			t.Errorf("access log lacks %q:\n%s", want, x.logs.String())
		}
	}
}

func TestPayloadErrors(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.MaxObjectSize = 10 })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	body := []byte("data")
	crc := crc32.ChecksumIEEE(body)
	crcB64 := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})

	x.mustDo(http.MethodPut, "/bkt/ok", body, map[string]string{"X-Amz-Checksum-Crc32": crcB64}, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/sdk", body, map[string]string{"X-Amz-Sdk-Checksum-Algorithm": "SHA256"}, http.StatusOK)
	cases := []struct {
		name string
		hdr  map[string]string
		body []byte
		code string
	}{
		{"md5 mismatch", map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(make([]byte, 16))}, body, "BadDigest"},
		{"md5 invalid", map[string]string{"Content-MD5": "nope"}, body, "InvalidDigest"},
		{"md5 empty", map[string]string{"Content-MD5": ""}, body, "InvalidDigest"},
		{"sha mismatch", map[string]string{"X-Amz-Content-Sha256": sha([]byte("other"))}, body, "XAmzContentSHA256Mismatch"},
		{"crc mismatch", map[string]string{"X-Amz-Checksum-Crc32": "AAAAAA=="}, body, "BadDigest"},
		{"trailer without chunks", map[string]string{"X-Amz-Trailer": "x-amz-checksum-crc32"}, body, "InvalidRequest"},
		{"unknown trailer", map[string]string{"X-Amz-Trailer": "x-amz-foo"}, body, "InvalidRequest"},
		{"too large", nil, bytes.Repeat([]byte("x"), 11), "EntityTooLarge"},
		{"no decoded length", map[string]string{"X-Amz-Content-Sha256": sigv4.StreamingUnsignedTrailer}, body, "MissingContentLength"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { x.expectErr(http.MethodPut, "/bkt/k", c.body, c.hdr, c.code) })
	}

	r := httptest.NewRequest(http.MethodPut, "http://pb.test/bkt/k", strings.NewReader("abc"))
	r.ContentLength = -1
	x.signer.Sign(r, sigv4.UnsignedPayload, testNow)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if errCode(rec) != "MissingContentLength" {
		t.Errorf("unknown length: %s", rec.Body.String())
	}

	// An HTTP chunked body of unknown length is accepted for a whole object, up to the
	// maximum size, but not for a part.
	chunked := func(target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "http://pb.test"+target, strings.NewReader(body))
		r.ContentLength, r.TransferEncoding = -1, []string{"chunked"}
		x.signer.Sign(r, sigv4.UnsignedPayload, testNow)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec
	}
	if rec := chunked("/bkt/te", "chunky"); rec.Code != http.StatusOK {
		t.Errorf("chunked put: %d %s", rec.Code, rec.Body.String())
	}
	if got := x.mustDo(http.MethodGet, "/bkt/te", nil, nil, http.StatusOK).Body.String(); got != "chunky" {
		t.Errorf("chunked body = %q", got)
	}
	if rec := chunked("/bkt/te-big", strings.Repeat("x", 11)); errCode(rec) != "EntityTooLarge" {
		t.Errorf("chunked over the maximum: %d %s", rec.Code, rec.Body.String())
	}
	x.mustDo(http.MethodGet, "/bkt/te-big", nil, nil, http.StatusNotFound)
	if rec := chunked("/bkt/te?partNumber=1&uploadId=x", "part"); errCode(rec) != "MissingContentLength" {
		t.Errorf("chunked part: %s", rec.Body.String())
	}

	r = httptest.NewRequest(http.MethodPut, "http://pb.test/bkt/k", io.MultiReader(strings.NewReader("ab"), errReader{}))
	r.ContentLength = 4
	x.signer.Sign(r, sigv4.UnsignedPayload, testNow)
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if errCode(rec) != "IncompleteBody" {
		t.Errorf("broken body: %s", rec.Body.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

// chunked builds a signed aws-chunked body with a CRC32 trailer.
func (x *harness) chunked(seed string, data []byte, trailer string) string {
	var b strings.Builder
	prev := seed
	for _, c := range [][]byte{data, nil} {
		prev = x.signer.ChunkSignature(testNow, prev, c)
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(c), prev)
		if len(c) > 0 {
			b.Write(c)
			b.WriteString("\r\n")
		}
	}
	if trailer != "" {
		b.WriteString("x-amz-checksum-crc32:" + trailer + "\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

func TestChunkedUpload(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	data := []byte("chunked data")
	crc := crc32.ChecksumIEEE(data)
	good := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})

	send := func(trailerHeader, trailer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "http://pb.test/bkt/c.txt", nil)
		r.Header.Set("Content-Encoding", "aws-chunked,gzip")
		r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(data)))
		if trailerHeader != "" {
			r.Header.Set("X-Amz-Trailer", trailerHeader)
		}
		// The trailer signature is not covered here: an unsigned-trailer upload carries none.
		seed := x.signer.Sign(r, sigv4.StreamingSigned, testNow)
		body := x.chunked(seed, data, "")
		if trailer != "" {
			body = strings.TrimSuffix(body, "\r\n") + "x-amz-checksum-crc32:" + trailer + "\r\n\r\n"
		}
		r.Body = io.NopCloser(strings.NewReader(body))
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec
	}
	rec := send("", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("chunked put: %d %s", rec.Code, rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt/c.txt", nil, nil, http.StatusOK)
	if rec.Body.String() != "chunked data" || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("stored %q, Content-Encoding %q", rec.Body.String(), rec.Header().Get("Content-Encoding"))
	}
	// A trailer on a non-trailer payload is rejected by the decoder.
	if rec := send("x-amz-checksum-crc32", good); errCode(rec) != "InvalidArgument" {
		t.Errorf("trailer on STREAMING-AWS4-HMAC-SHA256-PAYLOAD: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnsignedTrailerUpload(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	data := []byte("trailer data")
	crc := crc32.ChecksumIEEE(data)
	good := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	send := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "http://pb.test/bkt/t.txt", strings.NewReader(body))
		r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(data)))
		r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		x.signer.Sign(r, sigv4.StreamingUnsignedTrailer, testNow)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec
	}
	frame := fmt.Sprintf("%x\r\n%s\r\n0\r\n", len(data), data)
	if rec := send(frame + "x-amz-checksum-crc32:" + good + "\r\n\r\n"); rec.Code != http.StatusOK || rec.Header().Get("X-Amz-Checksum-Crc32") != good {
		t.Fatalf("good trailer: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := send(frame + "x-amz-checksum-crc32:AAAAAA==\r\n\r\n"); errCode(rec) != "BadDigest" {
		t.Errorf("bad trailer: %s", rec.Body.String())
	}
	if rec := send(frame + "\r\n"); errCode(rec) != "InvalidRequest" {
		t.Errorf("missing trailer: %s", rec.Body.String())
	}
}

func TestListObjectsAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	for _, k := range []string{"a b/1", "a b/2", "c", "d+e"} {
		x.mustDo(http.MethodPut, "/bkt/"+strings.ReplaceAll(k, " ", "%20"), []byte("x"), nil, http.StatusOK)
	}
	rec := x.mustDo(http.MethodGet, "/bkt?list-type=2&delimiter=/&encoding-type=url&max-keys=2&fetch-owner=true", nil, nil, http.StatusOK)
	var res listBucketResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0].Prefix != "a%20b/" || len(res.Contents) != 1 ||
		res.Contents[0].Owner == nil || !res.IsTruncated || res.NextContinuationToken == "" || *res.KeyCount != 2 || res.EncodingType != "url" {
		t.Fatalf("page 1: %s", rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt?list-type=2&delimiter=/&encoding-type=url&continuation-token="+res.NextContinuationToken, nil, nil, http.StatusOK)
	res = listBucketResult{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Contents) != 1 || res.Contents[0].Key != "d%2Be" || res.IsTruncated || res.Contents[0].Owner != nil {
		t.Errorf("page 2: %s", rec.Body.String())
	}

	rec = x.mustDo(http.MethodGet, "/bkt?marker=c&max-keys=5000", nil, nil, http.StatusOK)
	res = listBucketResult{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Contents) != 1 || res.Contents[0].Key != "d+e" || res.MaxKeys != 1000 || res.Marker == nil || *res.Marker != "c" {
		t.Errorf("v1: %s", rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt?max-keys=1", nil, nil, http.StatusOK)
	res = listBucketResult{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if res.NextMarker != "a b/1" || !res.IsTruncated {
		t.Errorf("v1 next marker: %s", rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt?list-type=2&start-after=c", nil, nil, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "<StartAfter>c</StartAfter>") {
		t.Errorf("start-after: %s", rec.Body.String())
	}

	rec = x.mustDo(http.MethodGet, "/bkt?prefix=%0A&encoding-type=url", nil, nil, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "<Prefix>&#xA;</Prefix>") {
		t.Errorf("V1 echoes the prefix unencoded: %s", rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt?list-type=2&prefix=%0A&encoding-type=url&continuation-token=", nil, nil, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, "<Prefix>%0A</Prefix>") || !strings.Contains(body, "<ContinuationToken></ContinuationToken>") {
		t.Errorf("V2 prefix and empty token: %s", body)
	}

	x.expectErr(http.MethodGet, "/bkt?max-keys=-1", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt?encoding-type=gzip", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt?list-type=2&continuation-token=%25%25", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/missing?list-type=2", nil, nil, "NoSuchBucket")
}

func TestListObjectVersionsAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	for _, k := range []string{"a/1", "a/2", "b", "c d"} {
		x.mustDo(http.MethodPut, "/bkt/"+strings.ReplaceAll(k, " ", "%20"), []byte("xy"), nil, http.StatusOK)
	}
	page := func(target string) listVersionsResult {
		t.Helper()
		var res listVersionsResult
		if err := xml.Unmarshal(x.mustDo(http.MethodGet, target, nil, nil, http.StatusOK).Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := page("/bkt?versions&delimiter=/&max-keys=2")
	if len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0].Prefix != "a/" || len(res.Versions) != 1 {
		t.Fatalf("page 1: %+v", res)
	}
	v := res.Versions[0]
	if v.Key != "b" || v.VersionID != "null" || !v.IsLatest || v.Size != 2 || v.Owner == nil || v.StorageClass != "STANDARD" {
		t.Errorf("version: %+v", v)
	}
	if !res.IsTruncated || res.NextKeyMarker != "b" || res.NextVersionIDMarker != "null" {
		t.Errorf("page 1 markers: %+v", res)
	}
	res = page("/bkt?versions&delimiter=/&key-marker=b&version-id-marker=null&encoding-type=url")
	if len(res.Versions) != 1 || res.Versions[0].Key != "c%20d" || res.IsTruncated || res.KeyMarker != "b" ||
		res.VersionIDMarker != "null" || res.EncodingType != "url" {
		t.Errorf("page 2: %+v", res)
	}
	res = page("/bkt?versions&max-keys=1&delimiter=/")
	if res.NextKeyMarker != "a/" || res.NextVersionIDMarker != "" {
		t.Errorf("a prefix ends the page without a version marker: %+v", res)
	}
	if res := page("/bkt?versions&prefix=a/"); len(res.Versions) != 2 || res.Prefix != "a/" {
		t.Errorf("prefix: %+v", res)
	}

	x.expectErr(http.MethodGet, "/bkt?versions&version-id-marker=null", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt?versions&key-marker=b&version-id-marker=abc", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt?versions&max-keys=x", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/missing?versions", nil, nil, "NoSuchBucket")
}

func TestCopyAPI(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.MaxObjectSize = 20 })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/src%20key", []byte("copy"), map[string]string{"Content-Type": "text/plain"}, http.StatusOK)

	rec := x.mustDo(http.MethodPut, "/bkt/dst", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src%20key?versionId=null"}, http.StatusOK)
	var cr copyObjectResult
	if xml.Unmarshal(rec.Body.Bytes(), &cr) != nil || cr.ETag == "" {
		t.Errorf("copy: %s", rec.Body.String())
	}
	rec = x.mustDo(http.MethodPut, "/bkt/dst", nil, map[string]string{
		"X-Amz-Copy-Source": "bkt/dst", "X-Amz-Metadata-Directive": "REPLACE", "Content-Type": "application/json",
	}, http.StatusOK)
	if got := x.mustDo(http.MethodHead, "/bkt/dst", nil, nil, http.StatusOK).Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("replaced content type = %q", got)
	}
	if cr = (copyObjectResult{}); xml.Unmarshal(rec.Body.Bytes(), &cr) != nil || cr.ETag == "" {
		t.Errorf("self copy with REPLACE: %s", rec.Body.String())
	}

	x.expectErr(http.MethodPut, "/bkt/dst", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst"}, "InvalidRequest")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst", "X-Amz-Metadata-Directive": "MOVE"}, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt"}, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/%zz"}, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst?versionId=v2"}, "NotImplemented")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/missing"}, "NoSuchKey")
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst", "X-Amz-Copy-Source-If-Match": `"nope"`}, "PreconditionFailed")
	x.mustDo(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst", "If-None-Match": `"other"`}, http.StatusOK)
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/dst", "If-None-Match": "*"}, "PreconditionFailed")

	if err := os.WriteFile(filepath.Join(x.root, "bkt", "huge"), bytes.Repeat([]byte("x"), 30), 0o644); err != nil {
		t.Fatal(err)
	}
	x.expectErr(http.MethodPut, "/bkt/x", nil, map[string]string{"X-Amz-Copy-Source": "bkt/huge"}, "EntityTooLarge")
}

func TestDeleteObjectsAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/a", []byte("x"), nil, http.StatusOK)
	doc := []byte(`<Delete><Object><Key>a</Key></Object><Object><Key>b</Key><VersionId>v1</VersionId></Object></Delete>`)
	rec := x.mustDo(http.MethodPost, "/bkt?delete", doc, nil, http.StatusOK)
	var res deleteResult
	if xml.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res.Deleted) != 1 || len(res.Errors) != 1 || res.Errors[0].Code != "NotImplemented" {
		t.Errorf("delete result: %s", rec.Body.String())
	}
	quiet := []byte(`<Delete><Quiet>true</Quiet><Object><Key>a</Key></Object></Delete>`)
	rec = x.mustDo(http.MethodPost, "/bkt?delete", quiet, nil, http.StatusOK)
	if strings.Contains(rec.Body.String(), "<Deleted>") {
		t.Errorf("quiet: %s", rec.Body.String())
	}
	x.expectErr(http.MethodPost, "/bkt?delete", []byte("<Delete>"), nil, "MalformedXML")
	x.expectErr(http.MethodPost, "/bkt?delete", []byte("<Delete></Delete>"), nil, "MalformedXML")
	x.expectErr(http.MethodPost, "/missing?delete", quiet, nil, "NoSuchBucket")
	x.expectErr(http.MethodPost, "/bkt?delete", bytes.Repeat([]byte("x"), maxXMLBody+1), nil, "InvalidRequest")
}

func TestMultipartAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	rec := x.mustDo(http.MethodPost, "/bkt/big?uploads", nil, map[string]string{"Content-Type": "text/csv", "X-Amz-Checksum-Algorithm": "CRC32"}, http.StatusOK)
	var init initiateMultipartUploadResult
	if xml.Unmarshal(rec.Body.Bytes(), &init) != nil || init.UploadID == "" || rec.Header().Get("X-Amz-Checksum-Algorithm") != "CRC32" {
		t.Fatalf("initiate: %s", rec.Body.String())
	}
	id := init.UploadID
	var etags []string
	for i, part := range []string{"aaaaa", "bbbbb", "c"} {
		rec := x.mustDo(http.MethodPut, fmt.Sprintf("/bkt/big?partNumber=%d&uploadId=%s", i+1, id), []byte(part), nil, http.StatusOK)
		etags = append(etags, rec.Header().Get("ETag"))
	}
	x.expectErr(http.MethodPut, "/bkt/big?partNumber=x&uploadId="+id, []byte("a"), nil, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/big?partNumber=0&uploadId="+id, []byte("a"), nil, "InvalidArgument")
	x.expectErr(http.MethodPut, "/bkt/big?partNumber=1&uploadId=nope", []byte("a"), nil, "NoSuchUpload")

	rec = x.mustDo(http.MethodGet, "/bkt/big?uploadId="+id+"&max-parts=2", nil, nil, http.StatusOK)
	var lp listPartsResult
	if xml.Unmarshal(rec.Body.Bytes(), &lp) != nil || len(lp.Parts) != 2 || !lp.IsTruncated || lp.NextPartNumberMarker != 2 {
		t.Errorf("list parts: %s", rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/bkt/big?uploadId="+id+"&max-parts=-1", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/big?uploadId="+id+"&part-number-marker=x", nil, nil, "InvalidArgument")

	rec = x.mustDo(http.MethodGet, "/bkt?uploads&max-uploads=1", nil, nil, http.StatusOK)
	var lu listMultipartUploadsResult
	if xml.Unmarshal(rec.Body.Bytes(), &lu) != nil || len(lu.Uploads) != 1 || lu.Uploads[0].UploadID != id {
		t.Errorf("list uploads: %s", rec.Body.String())
	}
	var other initiateMultipartUploadResult
	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/other?uploads", nil, nil, http.StatusOK).Body.Bytes(), &other)
	rec = x.mustDo(http.MethodGet, "/bkt?uploads&max-uploads=1", nil, nil, http.StatusOK)
	lu = listMultipartUploadsResult{}
	if xml.Unmarshal(rec.Body.Bytes(), &lu) != nil || !lu.IsTruncated || lu.NextKeyMarker != "big" {
		t.Errorf("list uploads truncated: %s", rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/bkt?uploads&max-uploads=x", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt?uploads&encoding-type=bogus", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/missing?uploads", nil, nil, "NoSuchBucket")

	var spaced initiateMultipartUploadResult
	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/a%20dir/x%2By?uploads", nil, nil, http.StatusOK).Body.Bytes(), &spaced)
	rec = x.mustDo(http.MethodGet, "/bkt?uploads&encoding-type=url&prefix=a%20dir/", nil, nil, http.StatusOK)
	lu = listMultipartUploadsResult{}
	if xml.Unmarshal(rec.Body.Bytes(), &lu) != nil || len(lu.Uploads) != 1 || lu.Uploads[0].Key != "a%20dir/x%2By" ||
		lu.Prefix != "a%20dir/" || lu.EncodingType != "url" {
		t.Errorf("list uploads encoded: %s", rec.Body.String())
	}
	x.mustDo(http.MethodDelete, "/bkt/a%20dir/x%2By?uploadId="+spaced.UploadID, nil, nil, http.StatusNoContent)

	x.expectErr(http.MethodPost, "/bkt/big?uploadId="+id, []byte("<nope"), nil, "MalformedXML")
	var doc strings.Builder
	doc.WriteString("<CompleteMultipartUpload>")
	for i, e := range etags {
		fmt.Fprintf(&doc, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, e)
	}
	doc.WriteString("</CompleteMultipartUpload>")

	r := httptest.NewRequest(http.MethodPost, "https://pb.test/bkt/big?uploadId="+id, strings.NewReader(doc.String()))
	r.TLS = &tls.ConnectionState{}
	x.signer.Sign(r, sha([]byte(doc.String())), testNow)
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	var done completeMultipartUploadResult
	if rec.Code != http.StatusOK || xml.Unmarshal(rec.Body.Bytes(), &done) != nil ||
		!strings.HasSuffix(done.ETag, `-3"`) || done.Location != "https://pb.test/bkt/big" {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	rec = x.mustDo(http.MethodGet, "/bkt/big", nil, nil, http.StatusOK)
	if rec.Body.String() != "aaaaabbbbbc" || rec.Header().Get("Content-Type") != "text/csv" {
		t.Errorf("assembled %q %v", rec.Body.String(), rec.Header())
	}

	x.mustDo(http.MethodDelete, "/bkt/other?uploadId="+other.UploadID, nil, nil, http.StatusNoContent)
	rec = x.mustDo(http.MethodGet, "/bkt?uploads", nil, nil, http.StatusOK)
	if strings.Contains(rec.Body.String(), "<Upload>") {
		t.Errorf("uploads left: %s", rec.Body.String())
	}
	x.expectErr(http.MethodDelete, "/bkt/other?uploadId="+id, nil, nil, "NoSuchUpload")
	x.expectErr(http.MethodPost, "/bkt/m/?uploads", nil, nil, "InvalidArgument")
}

// rawHeader returns the header sent under exactly name, which need not be canonical.
func rawHeader(h http.Header, name string) string {
	for k, v := range h {
		if k == name && len(v) == 1 {
			return v[0]
		}
	}
	return ""
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestMultipartChecksumAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	const partSHA = "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0="
	const composite = "Ok6Cs5b96ux6+MWQkJO7UBT5sKPBeXBLwvj/hK89smg=-1"
	start := func(key string) string {
		t.Helper()
		rec := x.mustDo(http.MethodPost, "/bkt/"+key+"?uploads", nil, map[string]string{"X-Amz-Checksum-Algorithm": "sha256"}, http.StatusOK)
		if rec.Header().Get("X-Amz-Checksum-Algorithm") != "SHA256" || rec.Header().Get("X-Amz-Checksum-Type") != "COMPOSITE" {
			t.Errorf("create headers: %v", rec.Header())
		}
		var res initiateMultipartUploadResult
		_ = xml.Unmarshal(rec.Body.Bytes(), &res)
		x.mustDo(http.MethodPut, "/bkt/"+key+"?partNumber=1&uploadId="+res.UploadID, []byte(strings.Repeat("A", 1024)),
			map[string]string{"X-Amz-Checksum-Sha256": partSHA}, http.StatusOK)
		return res.UploadID
	}
	doc := func(partChecksum string) []byte {
		return []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"` + md5hex(strings.Repeat("A", 1024)) +
			`"</ETag>` + partChecksum + `</Part></CompleteMultipartUpload>`)
	}

	id := start("k")
	x.expectErr(http.MethodPost, "/bkt/k?uploadId="+id, doc(""), map[string]string{"X-Amz-Checksum-Sha256": "bad"}, "BadDigest")
	rec := x.mustDo(http.MethodPost, "/bkt/k?uploadId="+id, doc("<ChecksumSHA256>"+partSHA+"</ChecksumSHA256>"),
		map[string]string{"X-Amz-Checksum-Sha256": composite}, http.StatusOK)
	var res completeMultipartUploadResult
	if xml.Unmarshal(rec.Body.Bytes(), &res) != nil || res.ChecksumSHA256 != composite || res.ChecksumType != "COMPOSITE" {
		t.Errorf("complete: %s", rec.Body.String())
	}
	if h := x.mustDo(http.MethodHead, "/bkt/k", nil, nil, http.StatusOK).Header(); h.Get("X-Amz-Checksum-Sha256") != "" {
		t.Errorf("checksum without checksum mode: %v", h)
	}
	h := x.mustDo(http.MethodHead, "/bkt/k", nil, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}, http.StatusOK).Header()
	if h.Get("X-Amz-Checksum-Sha256") != composite || h.Get("X-Amz-Checksum-Type") != "COMPOSITE" {
		t.Errorf("checksum mode: %v", h)
	}

	h = x.mustDo(http.MethodGet, "/bkt/k?partNumber=1", nil, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}, http.StatusPartialContent).Header()
	if h.Get("X-Amz-Checksum-Sha256") != partSHA || h.Get("X-Amz-Checksum-Type") != "COMPOSITE" {
		t.Errorf("part checksum: %v", h)
	}

	id = start("k2")
	x.expectErr(http.MethodPost, "/bkt/k2?uploadId="+id, doc("<ChecksumSHA256>other</ChecksumSHA256>"), nil, "InvalidPart")
	x.expectErr(http.MethodPost, "/bkt/k2?uploadId="+id, doc(""), map[string]string{"X-Amz-Checksum-Crc32": "AAAAAA=="}, "InvalidRequest")

	// A full-object CRC32 is computed over the data, and a copy keeps it.
	rec = x.mustDo(http.MethodPost, "/bkt/full?uploads", nil,
		map[string]string{"X-Amz-Checksum-Algorithm": "CRC32", "X-Amz-Checksum-Type": "FULL_OBJECT"}, http.StatusOK)
	var up initiateMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &up)
	x.mustDo(http.MethodPut, "/bkt/full?partNumber=1&uploadId="+up.UploadID, []byte("foo"), nil, http.StatusOK)
	rec = x.mustDo(http.MethodPost, "/bkt/full?uploadId="+up.UploadID, []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"`+
		md5hex("foo")+`"</ETag></Part></CompleteMultipartUpload>`), map[string]string{"X-Amz-Checksum-Crc32": "jHNlIQ=="}, http.StatusOK)
	res = completeMultipartUploadResult{}
	if xml.Unmarshal(rec.Body.Bytes(), &res) != nil || res.ChecksumCRC32 != "jHNlIQ==" || res.ChecksumType != "FULL_OBJECT" {
		t.Errorf("full object: %s", rec.Body.String())
	}
	x.mustDo(http.MethodPut, "/bkt/full-copy", nil, map[string]string{"X-Amz-Copy-Source": "bkt/full"}, http.StatusOK)
	h = x.mustDo(http.MethodHead, "/bkt/full-copy", nil, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}, http.StatusOK).Header()
	if h.Get("X-Amz-Checksum-Crc32") != "jHNlIQ==" || h.Get("X-Amz-Checksum-Type") != "FULL_OBJECT" {
		t.Errorf("copy keeps the full-object checksum: %v", h)
	}

	x.expectErr(http.MethodPost, "/bkt/k3?uploads", nil, map[string]string{"X-Amz-Checksum-Type": "COMPOSITE"}, "InvalidRequest")
	x.expectErr(http.MethodPost, "/bkt/k3?uploads", nil, map[string]string{"X-Amz-Checksum-Algorithm": "MD5"}, "InvalidRequest")
	x.expectErr(http.MethodPost, "/bkt/k3?uploads", nil,
		map[string]string{"X-Amz-Checksum-Algorithm": "SHA256", "X-Amz-Checksum-Type": "FULL_OBJECT"}, "InvalidRequest")
	// DeleteObjects still verifies its document's checksum.
	x.expectErr(http.MethodPost, "/bkt?delete", []byte(`<Delete><Object><Key>k</Key></Object></Delete>`),
		map[string]string{"X-Amz-Checksum-Crc32": "AAAAAA=="}, "BadDigest")
}

// multipartObject completes key from parts and returns the object's ETag.
func (x *harness) multipartObject(key string, parts ...string) string {
	x.t.Helper()
	rec := x.mustDo(http.MethodPost, "/bkt/"+key+"?uploads", nil, nil, http.StatusOK)
	var up initiateMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &up)
	doc := "<CompleteMultipartUpload>"
	for i, p := range parts {
		n := strconv.Itoa(i + 1)
		etag := x.mustDo(http.MethodPut, "/bkt/"+key+"?partNumber="+n+"&uploadId="+up.UploadID, []byte(p), nil, http.StatusOK).Header().Get("ETag")
		doc += "<Part><PartNumber>" + n + "</PartNumber><ETag>" + etag + "</ETag></Part>"
	}
	rec = x.mustDo(http.MethodPost, "/bkt/"+key+"?uploadId="+up.UploadID, []byte(doc+"</CompleteMultipartUpload>"), nil, http.StatusOK)
	var res completeMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	return res.ETag
}

func TestGetObjectPartNumber(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	etag := x.multipartObject("mp", "aaaaa", "bbbbb", "cc")
	for n, want := range []string{"aaaaa", "bbbbb", "cc"} {
		rec := x.mustDo(http.MethodGet, "/bkt/mp?partNumber="+strconv.Itoa(n+1), nil, nil, http.StatusPartialContent)
		if rec.Body.String() != want || rec.Header().Get("X-Amz-Mp-Parts-Count") != "3" || rec.Header().Get("ETag") != etag {
			t.Errorf("part %d: %q %v", n+1, rec.Body.String(), rec.Header())
		}
	}
	rec := x.mustDo(http.MethodHead, "/bkt/mp?partNumber=3", nil, nil, http.StatusPartialContent)
	if rec.Header().Get("Content-Length") != "2" || rec.Header().Get("Content-Range") != "bytes 10-11/12" {
		t.Errorf("head part: %v", rec.Header())
	}
	x.expectErr(http.MethodGet, "/bkt/mp?partNumber=4", nil, nil, "InvalidPartNumber")
	x.expectErr(http.MethodGet, "/bkt/mp?partNumber=0", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/mp?partNumber=x", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/mp?partNumber=1", nil, map[string]string{"Range": "bytes=0-1"}, "InvalidRequest")
	x.expectErr(http.MethodGet, "/bkt/missing?partNumber=1", nil, nil, "NoSuchKey")

	x.mustDo(http.MethodPut, "/bkt/single", []byte("body"), nil, http.StatusOK)
	rec = x.mustDo(http.MethodGet, "/bkt/single?partNumber=1", nil, nil, http.StatusPartialContent)
	if rec.Body.String() != "body" || rec.Header().Get("X-Amz-Mp-Parts-Count") != "" {
		t.Errorf("single part: %q %v", rec.Body.String(), rec.Header())
	}
	x.expectErr(http.MethodGet, "/bkt/single?partNumber=2", nil, nil, "InvalidPartNumber")
	x.mustDo(http.MethodPut, "/bkt/empty", nil, nil, http.StatusOK)
	x.mustDo(http.MethodGet, "/bkt/empty?partNumber=1", nil, nil, http.StatusOK)

	x.expectErr(http.MethodGet, "/bkt/empty", nil, map[string]string{"Range": "bytes=0-"}, "InvalidRange")
	x.expectErr(http.MethodGet, "/bkt/single", nil, map[string]string{"Range": "bytes=4-9"}, "InvalidRange")
	x.expectErr(http.MethodGet, "/bkt/single", nil, map[string]string{"Range": "bytes=-0"}, "InvalidRange")
	x.mustDo(http.MethodGet, "/bkt/single", nil, map[string]string{"Range": "bytes=-9"}, http.StatusPartialContent)
	x.mustDo(http.MethodGet, "/bkt/single", nil, map[string]string{"Range": "bytes=0-1,3-"}, http.StatusPartialContent)
	x.mustDo(http.MethodGet, "/bkt/single", nil, map[string]string{"Range": "items=0-1"}, http.StatusOK)

	x.multipartObject("tail", "aaaaa", "")
	if rec := x.mustDo(http.MethodGet, "/bkt/tail?partNumber=2", nil, nil, http.StatusOK); rec.Body.Len() != 0 {
		t.Errorf("an empty last part is empty, got %q", rec.Body.String())
	}
	if rec := x.do(http.MethodGet, "/bkt/mp?partNumber=9", nil, nil); rec.Header().Get("X-Amz-Mp-Parts-Count") != "" {
		t.Errorf("parts count on an error: %v", rec.Header())
	}
}

func TestGetObjectAttributes(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	etag := x.multipartObject("mp", "aaaaa", "bbbbb", "cc")
	all := map[string]string{"X-Amz-Object-Attributes": "ETag,Checksum, ObjectParts,StorageClass,ObjectSize"}
	get := func(key string, hdr map[string]string) objectAttributesResult {
		t.Helper()
		rec := x.mustDo(http.MethodGet, "/bkt/"+key+"?attributes", nil, hdr, http.StatusOK)
		if rec.Header().Get("Last-Modified") == "" {
			t.Error("no Last-Modified")
		}
		var res objectAttributesResult
		if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := get("mp", all)
	op := res.ObjectParts
	if res.ETag != strings.Trim(etag, `"`) || res.StorageClass != "STANDARD" || res.ObjectSize == nil || *res.ObjectSize != 12 ||
		op == nil || op.TotalPartsCount != 3 || len(op.Parts) != 3 || op.Parts[2].PartNumber != 3 || op.Parts[2].Size != 2 || op.IsTruncated {
		t.Errorf("multipart: %+v %+v", res, op)
	}
	paged := map[string]string{"X-Amz-Object-Attributes": "ObjectParts", "X-Amz-Max-Parts": "1", "X-Amz-Part-Number-Marker": "1"}
	res = get("mp", paged)
	if op = res.ObjectParts; res.ETag != "" || len(op.Parts) != 1 || op.Parts[0].PartNumber != 2 || !op.IsTruncated ||
		op.NextPartNumberMarker != 2 || op.PartNumberMarker != 1 || op.MaxParts != 1 {
		t.Errorf("paged: %+v", op)
	}

	x.mustDo(http.MethodPut, "/bkt/single", []byte("foo"), map[string]string{"X-Amz-Checksum-Crc32": "jHNlIQ=="}, http.StatusOK)
	res = get("single", all)
	if res.ObjectParts != nil || res.Checksum == nil || res.Checksum.ChecksumCRC32 != "jHNlIQ==" || res.Checksum.ChecksumType != "FULL_OBJECT" {
		t.Errorf("single: %+v %+v", res, res.Checksum)
	}

	res = get("mp", map[string]string{"X-Amz-Object-Attributes": "ObjectParts", "X-Amz-Max-Parts": "0"})
	if len(res.ObjectParts.Parts) != 3 || res.ObjectParts.IsTruncated {
		t.Errorf("max-parts 0 is the default: %+v", res.ObjectParts)
	}

	x.expectErr(http.MethodGet, "/bkt/mp?attributes", nil, nil, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/mp?attributes", nil, map[string]string{"X-Amz-Object-Attributes": "Owner"}, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/mp?attributes", nil, map[string]string{"X-Amz-Object-Attributes": "ETag", "X-Amz-Max-Parts": "-1"}, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/mp?attributes", nil, map[string]string{"X-Amz-Object-Attributes": "ETag", "X-Amz-Part-Number-Marker": "x"}, "InvalidArgument")
	x.expectErr(http.MethodGet, "/bkt/missing?attributes", nil, all, "NoSuchKey")
}

func TestMultipartSizeLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.MaxObjectSize = 8 })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	rec := x.mustDo(http.MethodPost, "/bkt/big?uploads", nil, nil, http.StatusOK)
	var init initiateMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &init)
	x.expectErr(http.MethodPut, "/bkt/big?partNumber=1&uploadId="+init.UploadID, []byte("123456789"), nil, "EntityTooLarge")
	e1 := x.mustDo(http.MethodPut, "/bkt/big?partNumber=1&uploadId="+init.UploadID, []byte("12345"), nil, http.StatusOK).Header().Get("ETag")
	e2 := x.mustDo(http.MethodPut, "/bkt/big?partNumber=2&uploadId="+init.UploadID, []byte("12345"), nil, http.StatusOK).Header().Get("ETag")
	doc := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", e1, e2)
	x.expectErr(http.MethodPost, "/bkt/big?uploadId="+init.UploadID, []byte(doc), nil, "EntityTooLarge")
	x.mustDo(http.MethodHead, "/bkt/big", nil, nil, http.StatusNotFound)
}

func TestInternalError(t *testing.T) {
	x := newHarness(t, nil)
	if err := os.RemoveAll(x.root); err != nil {
		t.Fatal(err)
	}
	x.expectErr(http.MethodGet, "/", nil, nil, "InternalError")
	if !strings.Contains(x.logs.String(), "request failed") {
		t.Error("internal error not logged")
	}
}

func TestToS3Unknown(t *testing.T) {
	if toS3(io.EOF) != nil {
		t.Error("unknown error mapped")
	}
	if e := toS3(fmt.Errorf("wrapped: %w", storage.ErrInvalidPartNumber)); e == nil || e.Code != "InvalidArgument" {
		t.Errorf("wrapped storage error: %v", e)
	}
}

func TestStripAWSChunked(t *testing.T) {
	for in, want := range map[string]string{"aws-chunked": "", "aws-chunked, gzip": "gzip", "gzip,br": "gzip, br", "": ""} {
		if got := stripAWSChunked(in); got != want {
			t.Errorf("stripAWSChunked(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPartChecksums(t *testing.T) {
	sums := map[string]string{"CRC32": "a", "CRC32C": "b", "CRC64NVME": "c", "SHA1": "d", "SHA256": "e"}
	p := partItem{checksumFields: newChecksumFields(sums)}
	if p.ChecksumCRC32 != "a" || p.ChecksumCRC32C != "b" || p.ChecksumCRC64NVME != "c" || p.ChecksumSHA1 != "d" || p.ChecksumSHA256 != "e" {
		t.Errorf("part = %+v", p)
	}
	if got := p.values(); len(got) != 5 || got["SHA256"] != "e" {
		t.Errorf("values = %v", got)
	}
}

func TestUploadPartCopyAPI(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = append(o.Credentials, Credential{AccessKey: "SCOPED", SecretKey: "scoped-secret", Buckets: []string{"bkt"}})
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/other", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/src", []byte("0123456789"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/other/src", []byte("secret"), nil, http.StatusOK)

	var up struct{ UploadId string }
	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/dst?uploads", nil, nil, http.StatusOK).Body.Bytes(), &up)
	part := func(n int, rng string) *httptest.ResponseRecorder {
		hdr := map[string]string{"X-Amz-Copy-Source": "/bkt/src"}
		if rng != "" {
			hdr["X-Amz-Copy-Source-Range"] = rng
		}
		return x.do(http.MethodPut, fmt.Sprintf("/bkt/dst?partNumber=%d&uploadId=%s", n, up.UploadId), nil, hdr)
	}
	var res struct {
		XMLName xml.Name
		ETag    string
	}
	rec := part(1, "bytes=0-4")
	if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.XMLName.Local != "CopyPartResult" || res.ETag == "" {
		t.Fatalf("copy part: %d %s", rec.Code, rec.Body.String())
	}
	e1 := res.ETag
	_ = xml.Unmarshal(part(2, "bytes=5-9").Body.Bytes(), &res)
	doc := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + e1 + `</ETag></Part><Part><PartNumber>2</PartNumber><ETag>` + res.ETag + `</ETag></Part></CompleteMultipartUpload>`
	x.mustDo(http.MethodPost, "/bkt/dst?uploadId="+up.UploadId, []byte(doc), nil, http.StatusOK)
	if got := x.mustDo(http.MethodGet, "/bkt/dst", nil, nil, http.StatusOK).Body.String(); got != "0123456789" {
		t.Errorf("assembled %q", got)
	}

	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/dst?uploads", nil, nil, http.StatusOK).Body.Bytes(), &up)
	for rng, code := range map[string]string{"bytes=5-10": "InvalidArgument", "bytes=5": "InvalidArgument", "0-4": "InvalidArgument", "bytes=4-1": "InvalidArgument"} {
		if got := errCode(part(1, rng)); got != code {
			t.Errorf("range %q: %s, want %s", rng, got, code)
		}
	}
	x.expectErr(http.MethodPut, "/bkt/dst?partNumber=1&uploadId="+up.UploadId, nil, map[string]string{"X-Amz-Copy-Source": "/bkt/missing"}, "NoSuchKey")
	x.as("SCOPED", "scoped-secret", func() {
		x.expectErr(http.MethodPut, "/bkt/dst?partNumber=1&uploadId="+up.UploadId, nil, map[string]string{"X-Amz-Copy-Source": "/other/src"}, "AccessDenied")
	})
}

func TestUploadPartCopySizeLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.MaxObjectSize = 4 })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/src", []byte("1234"), nil, http.StatusOK)
	var up struct{ UploadId string }
	_ = xml.Unmarshal(x.mustDo(http.MethodPost, "/bkt/dst?uploads", nil, nil, http.StatusOK).Body.Bytes(), &up)
	target := "/bkt/dst?partNumber=1&uploadId=" + up.UploadId
	x.mustDo(http.MethodPut, target, nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src"}, http.StatusOK)
	x.expectErr(http.MethodPut, target, nil, map[string]string{"X-Amz-Copy-Source": "/bkt/src", "X-Amz-Copy-Source-Range": "bytes=0-9"}, "EntityTooLarge")
	x.mustDo(http.MethodPut, "/bkt/big", []byte("1234"), nil, http.StatusOK)
	h := x.h
	h.opts.MaxObjectSize = 3
	x.expectErr(http.MethodPut, target, nil, map[string]string{"X-Amz-Copy-Source": "/bkt/big"}, "EntityTooLarge")
}

func TestGetObjectTagging(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.PublicBuckets = []string{"bkt"} })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("x"), nil, http.StatusOK)
	rec := x.mustDo(http.MethodGet, "/bkt/k?tagging", nil, nil, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "<TagSet></TagSet>") {
		t.Errorf("tagging: %s", rec.Body.String())
	}
	x.expectErr(http.MethodGet, "/bkt/missing?tagging", nil, nil, "NoSuchKey")
	if rec := x.anon(http.MethodGet, "/bkt/k?tagging"); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous tagging: %d", rec.Code)
	}
}

func TestListBucketsPaginated(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = append(o.Credentials, Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"b-two"}})
	})
	for _, b := range []string{"a-one", "b-two", "b-three"} {
		x.mustDo(http.MethodPut, "/"+b, nil, nil, http.StatusOK)
	}
	page := func(query string) listAllMyBucketsResult {
		t.Helper()
		var res listAllMyBucketsResult
		if err := xml.Unmarshal(x.mustDo(http.MethodGet, "/"+query, nil, nil, http.StatusOK).Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	names := func(res listAllMyBucketsResult) (out []string) {
		for _, b := range res.Buckets {
			out = append(out, b.Name)
		}
		return out
	}

	if res := page(""); len(res.Buckets) != 3 || res.ContinuationToken != "" {
		t.Errorf("unpaginated: %+v", res)
	}
	var got []string
	token := ""
	for range 3 {
		q := "?max-buckets=2"
		if token != "" {
			q += "&continuation-token=" + token
		}
		res := page(q)
		got = append(got, names(res)...)
		if token = res.ContinuationToken; token == "" {
			break
		}
	}
	if strings.Join(got, ",") != "a-one,b-three,b-two" {
		t.Errorf("pages: %v", got)
	}
	if res := page("?max-buckets=3"); res.ContinuationToken != "" {
		t.Errorf("a full last page has no token: %+v", res)
	}
	if res := page("?prefix=b-"); strings.Join(names(res), ",") != "b-three,b-two" || res.Prefix != "b-" {
		t.Errorf("prefix: %+v", res)
	}
	if res := page("?bucket-region=eu-west-1"); len(res.Buckets) != 0 {
		t.Errorf("other region: %+v", res)
	}
	if res := page("?bucket-region=us-east-1"); len(res.Buckets) != 3 {
		t.Errorf("own region: %+v", res)
	}
	x.as("scoped", "scoped-secret", func() {
		if res := page("?max-buckets=1"); strings.Join(names(res), ",") != "b-two" || res.ContinuationToken != "" {
			t.Errorf("scoped: %+v", res)
		}
	})
	for _, q := range []string{"?max-buckets=0", "?max-buckets=10001", "?max-buckets=x", "?continuation-token=%21%21", "?continuation-token=QQ"} {
		x.expectErr(http.MethodGet, "/"+q, nil, nil, "InvalidArgument")
	}
}
