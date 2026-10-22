// Package compat runs the official AWS SDK for Go v2 against plainbucket. It is a separate
// module so the server itself keeps its standard-library-only dependency set.
package compat

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	accessKey = "plainbucket-test"
	secretKey = "plainbucket-test-secret/+="
)

type env struct {
	root   string
	url    string
	addr   string // listener address, for clients that dial a virtual host
	tls    bool
	client *s3.Client
	http   *http.Client
}

// setup starts plainbucket over plain HTTP or TLS: the SDK picks a different payload
// signing mode for each (signed payload over HTTP, aws-chunked with trailers over TLS).
func setup(t *testing.T, useTLS bool) *env { return setupWith(t, useTLS, nil) }

// setupWith is setup with server options changed by mutate.
func setupWith(t *testing.T, useTLS bool, mutate func(*s3api.Options)) *env {
	t.Helper()
	root := t.TempDir()
	store, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := s3api.Options{
		Store: store, Credentials: []s3api.Credential{{AccessKey: accessKey, SecretKey: secretKey}}, Region: "us-east-1",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	h := s3api.New(opts)
	srv := httptest.NewUnstartedServer(h)
	if useTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)

	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		HTTPClient:  srv.Client(),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	return &env{root: root, url: srv.URL, addr: srv.Listener.Addr().String(), tls: useTLS, client: client, http: srv.Client()}
}

func forEachTransport(t *testing.T, fn func(t *testing.T, e *env)) {
	for _, useTLS := range []bool{false, true} {
		name := "http"
		if useTLS {
			name = "https"
		}
		t.Run(name, func(t *testing.T) { fn(t, setup(t, useTLS)) })
	}
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func mustCreateBucket(t *testing.T, e *env, name string) {
	t.Helper()
	if _, err := e.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

func TestBuckets(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "alpha")
		_, err := e.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("alpha")})
		var owned *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &owned) {
			t.Errorf("duplicate bucket: %v", err)
		}
		if _, err := e.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("alpha")}); err != nil {
			t.Errorf("HeadBucket: %v", err)
		}
		if _, err := e.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("missing")}); err == nil {
			t.Error("HeadBucket on a missing bucket succeeded")
		}
		out, err := e.client.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil || len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "alpha" {
			t.Fatalf("ListBuckets: %+v %v", out, err)
		}
		loc, err := e.client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("alpha")})
		if err != nil || loc.LocationConstraint != "" {
			t.Errorf("GetBucketLocation: %+v %v", loc, err)
		}
		if _, err := e.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("alpha")}); err != nil {
			t.Errorf("DeleteBucket: %v", err)
		}
	})
}

func TestObjectRoundTrip(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "data")
		body := []byte("hello plainbucket, " + strings.Repeat("0123456789", 1000))
		put, err := e.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:       aws.String("data"),
			Key:          aws.String("docs/hello world+ü.txt"),
			Body:         bytes.NewReader(body),
			ContentType:  aws.String("text/plain"),
			CacheControl: aws.String("max-age=60"),
			Metadata:     map[string]string{"owner": "alice"},
		})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if aws.ToString(put.ETag) != `"`+md5hex(body)+`"` {
			t.Errorf("ETag = %s", aws.ToString(put.ETag))
		}

		onDisk, err := os.ReadFile(filepath.Join(e.root, "data", "docs", "hello world+ü.txt"))
		if err != nil || !bytes.Equal(onDisk, body) {
			t.Fatalf("object is not a plain file: %v", err)
		}

		get, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("data"), Key: aws.String("docs/hello world+ü.txt")})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		got, _ := io.ReadAll(get.Body)
		_ = get.Body.Close()
		if !bytes.Equal(got, body) || aws.ToString(get.ContentType) != "text/plain" ||
			aws.ToString(get.CacheControl) != "max-age=60" || get.Metadata["owner"] != "alice" {
			t.Errorf("GetObject: %d bytes, %+v", len(got), get)
		}

		rng, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("data"), Key: aws.String("docs/hello world+ü.txt"), Range: aws.String("bytes=6-15")})
		if err != nil {
			t.Fatalf("ranged GetObject: %v", err)
		}
		part, _ := io.ReadAll(rng.Body)
		_ = rng.Body.Close()
		if string(part) != "plainbucke" {
			t.Errorf("range = %q", part)
		}

		head, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("data"), Key: aws.String("docs/hello world+ü.txt"), ChecksumMode: types.ChecksumModeEnabled})
		if err != nil || aws.ToInt64(head.ContentLength) != int64(len(body)) {
			t.Fatalf("HeadObject: %+v %v", head, err)
		}
		// Over TLS the SDK sends CRC32; otherwise the server computes its default CRC64NVME.
		if aws.ToString(head.ChecksumCRC32) == "" && aws.ToString(head.ChecksumCRC64NVME) == "" {
			t.Errorf("HeadObject returned no checksum")
		}

		_, err = e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("data"), Key: aws.String("nope")})
		var nf *types.NotFound
		if !errors.As(err, &nf) {
			t.Errorf("HeadObject missing: %v", err)
		}
		_, err = e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("data"), Key: aws.String("nope")})
		var nsk *types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Errorf("GetObject missing: %v", err)
		}

		if _, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("data"), Key: aws.String("docs/hello world+ü.txt")}); err != nil {
			t.Errorf("DeleteObject: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.root, "data", "docs")); !os.IsNotExist(err) {
			t.Error("empty prefix directory kept after delete")
		}
	})
}

func TestChecksumAlgorithms(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "sums")
		for _, alg := range []types.ChecksumAlgorithm{
			types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmCrc64nvme,
			types.ChecksumAlgorithmSha1, types.ChecksumAlgorithmSha256,
		} {
			key := "obj-" + string(alg)
			_, err := e.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("sums"), Key: aws.String(key), Body: strings.NewReader("checksum me"), ChecksumAlgorithm: alg,
			})
			if err != nil {
				t.Errorf("PutObject %s: %v", alg, err)
				continue
			}
			out, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("sums"), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
			if err != nil {
				t.Errorf("GetObject %s: %v", alg, err)
				continue
			}
			// The SDK validates the returned checksum while reading the body.
			if _, err := io.ReadAll(out.Body); err != nil {
				t.Errorf("GetObject %s body: %v", alg, err)
			}
			_ = out.Body.Close()
		}
	})
}

// unseekable hides Seek so the SDK cannot pre-compute the payload hash.
type unseekable struct{ io.Reader }

func TestStreamingUpload(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	mustCreateBucket(t, e, "stream")
	body := bytes.Repeat([]byte("stream!"), 100000)
	_, err := e.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("stream"), Key: aws.String("s.bin"), Body: unseekable{bytes.NewReader(body)},
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	onDisk, _ := os.ReadFile(filepath.Join(e.root, "stream", "s.bin"))
	if !bytes.Equal(onDisk, body) {
		t.Errorf("stored %d bytes, want %d", len(onDisk), len(body))
	}
}

func TestMultipartUpload(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "big")
		body := make([]byte, 21<<20)
		for i := range body {
			body[i] = byte(i * 7)
		}
		up := manager.NewUploader(e.client, func(u *manager.Uploader) { u.PartSize = 5 << 20; u.Concurrency = 2 })
		out, err := up.Upload(ctx, &s3.PutObjectInput{Bucket: aws.String("big"), Key: aws.String("blob.bin"), Body: bytes.NewReader(body)})
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if !strings.HasSuffix(aws.ToString(out.ETag), `-5"`) {
			t.Errorf("multipart ETag = %s", aws.ToString(out.ETag))
		}
		onDisk, _ := os.ReadFile(filepath.Join(e.root, "big", "blob.bin"))
		if md5hex(onDisk) != md5hex(body) {
			t.Fatal("assembled object differs from the upload")
		}

		dl := manager.NewDownloader(e.client, func(d *manager.Downloader) { d.PartSize = 5 << 20 })
		buf := manager.NewWriteAtBuffer(nil)
		if _, err := dl.Download(ctx, buf, &s3.GetObjectInput{Bucket: aws.String("big"), Key: aws.String("blob.bin")}); err != nil {
			t.Fatalf("Download: %v", err)
		}
		if md5hex(buf.Bytes()) != md5hex(body) {
			t.Error("ranged download differs")
		}

		// An abandoned upload is listed, then aborted.
		cu, err := e.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("left")})
		if err != nil {
			t.Fatal(err)
		}
		lu, err := e.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("big")})
		if err != nil || len(lu.Uploads) != 1 || aws.ToString(lu.Uploads[0].UploadId) != aws.ToString(cu.UploadId) {
			t.Errorf("ListMultipartUploads: %+v %v", lu, err)
		}
		if _, err := e.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("big"), Key: aws.String("left"), UploadId: cu.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("part"),
		}); err != nil {
			t.Fatal(err)
		}
		lp, err := e.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("big"), Key: aws.String("left"), UploadId: cu.UploadId})
		if err != nil || len(lp.Parts) != 1 || aws.ToInt64(lp.Parts[0].Size) != 4 {
			t.Errorf("ListParts: %+v %v", lp, err)
		}
		if _, err := e.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("left"), UploadId: cu.UploadId}); err != nil {
			t.Errorf("Abort: %v", err)
		}
		_, err = e.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("big"), Key: aws.String("left"), UploadId: cu.UploadId})
		if apiCode(err) != "NoSuchUpload" {
			t.Errorf("ListParts after abort: %v", err)
		}
	})
}

func TestListObjectsV2(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "list")
		var want []string
		for i := range 1050 {
			k := fmt.Sprintf("logs/%02d/entry-%04d.txt", i%7, i)
			want = append(want, k)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(e.root, "list", k)), 0o755); err != nil {
				t.Fatal(err)
			}
			// Written straight to disk: plain files dropped in are objects too.
			if err := os.WriteFile(filepath.Join(e.root, "list", k), []byte(k), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("list"), Key: aws.String("readme a+b.md"), Body: strings.NewReader("x")})
		want = append(want, "readme a+b.md")

		var got []string
		p := s3.NewListObjectsV2Paginator(e.client, &s3.ListObjectsV2Input{Bucket: aws.String("list"), MaxKeys: aws.Int32(300)})
		pages := 0
		for p.HasMorePages() {
			out, err := p.NextPage(ctx)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			pages++
			for _, o := range out.Contents {
				got = append(got, aws.ToString(o.Key))
			}
		}
		if pages != 4 || len(got) != len(want) {
			t.Fatalf("%d pages, %d keys, want 4 pages, %d keys", pages, len(got), len(want))
		}
		for i := range got {
			if i > 0 && got[i-1] >= got[i] {
				t.Fatalf("keys out of order at %d: %q >= %q", i, got[i-1], got[i])
			}
		}

		out, err := e.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("list"), Prefix: aws.String("logs/"), Delimiter: aws.String("/")})
		if err != nil || len(out.CommonPrefixes) != 7 || len(out.Contents) != 0 {
			t.Fatalf("delimiter listing: %d prefixes, %d objects, %v", len(out.CommonPrefixes), len(out.Contents), err)
		}
		if aws.ToString(out.CommonPrefixes[0].Prefix) != "logs/00/" {
			t.Errorf("first prefix = %s", aws.ToString(out.CommonPrefixes[0].Prefix))
		}

		v1, err := e.client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String("list"), Prefix: aws.String("readme")})
		if err != nil || len(v1.Contents) != 1 || aws.ToString(v1.Contents[0].Key) != "readme a+b.md" {
			t.Errorf("ListObjects v1: %+v %v", v1, err)
		}
	})
}

func TestCopyAndBatchDelete(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "src")
		mustCreateBucket(t, e, "dst")
		_, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("src"), Key: aws.String("a b/c.txt"), Body: strings.NewReader("copy me"), ContentType: aws.String("text/plain")})
		if err != nil {
			t.Fatal(err)
		}
		cp, err := e.client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: aws.String("dst"), Key: aws.String("copied.txt"), CopySource: aws.String("src/a%20b/c.txt"),
		})
		if err != nil || aws.ToString(cp.CopyObjectResult.ETag) != `"`+md5hex([]byte("copy me"))+`"` {
			t.Fatalf("CopyObject: %+v %v", cp, err)
		}
		head, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("dst"), Key: aws.String("copied.txt")})
		if err != nil || aws.ToString(head.ContentType) != "text/plain" {
			t.Errorf("copied metadata: %+v %v", head, err)
		}

		for i := range 5 {
			_, _ = e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("dst"), Key: aws.String(fmt.Sprintf("del/%d", i)), Body: strings.NewReader("x")})
		}
		var ids []types.ObjectIdentifier
		for i := range 5 {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(fmt.Sprintf("del/%d", i))})
		}
		ids = append(ids, types.ObjectIdentifier{Key: aws.String("never-existed")})
		del, err := e.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("dst"), Delete: &types.Delete{Objects: ids}})
		if err != nil || len(del.Deleted) != 6 || len(del.Errors) != 0 {
			t.Fatalf("DeleteObjects: %+v %v", del, err)
		}
		if _, err := os.Stat(filepath.Join(e.root, "dst", "del")); !os.IsNotExist(err) {
			t.Error("prefix directory kept after batch delete")
		}
	})
}

func TestConditionalWrite(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "locks")
		in := func() *s3.PutObjectInput {
			return &s3.PutObjectInput{Bucket: aws.String("locks"), Key: aws.String("state.tflock"), Body: strings.NewReader("{}"), IfNoneMatch: aws.String("*")}
		}
		if _, err := e.client.PutObject(ctx, in()); err != nil {
			t.Fatalf("first lock: %v", err)
		}
		if _, err := e.client.PutObject(ctx, in()); apiCode(err) != "PreconditionFailed" {
			t.Errorf("second lock: %v", err)
		}
	})
}

func TestPresignedURLs(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "pre")
		ps := s3.NewPresignClient(e.client)
		put, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("pre"), Key: aws.String("up load.txt")}, s3.WithPresignExpires(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(put.Method, put.URL, strings.NewReader("presigned body"))
		for k, v := range put.SignedHeader {
			req.Header[k] = v
		}
		resp, err := e.http.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned PUT: %v %v", resp, err)
		}
		_ = resp.Body.Close()

		get, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("pre"), Key: aws.String("up load.txt")}, s3.WithPresignExpires(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		resp, err = e.http.Get(get.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "presigned body" {
			t.Errorf("presigned GET: %d %q", resp.StatusCode, b)
		}

		tampered := strings.Replace(get.URL, "up%20load", "other", 1)
		resp, err = e.http.Get(tampered)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("tampered URL: %d", resp.StatusCode)
			}
		}
	})
}

func TestExternalEditAndMarkers(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	mustCreateBucket(t, e, "edit")
	if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("edit"), Key: aws.String("f.txt"), Body: strings.NewReader("v1")}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.root, "edit", "f.txt")
	_ = os.WriteFile(p, []byte("edited outside"), 0o644)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, later, later)
	head, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("edit"), Key: aws.String("f.txt")})
	if err != nil || aws.ToString(head.ETag) != `"`+md5hex([]byte("edited outside"))+`"` {
		t.Errorf("ETag after external edit: %+v %v", head, err)
	}

	if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("edit"), Key: aws.String("folder/"), Body: strings.NewReader("")}); err != nil {
		t.Fatalf("folder marker: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(e.root, "edit", "folder")); err != nil || !fi.IsDir() {
		t.Error("folder marker is not a directory")
	}
	out, err := e.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("edit")})
	if err != nil || len(out.Contents) != 2 || aws.ToString(out.Contents[1].Key) != "folder/" {
		t.Errorf("listing with marker: %+v %v", out, err)
	}
	_, err = e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("edit"), Key: aws.String("f.txt/child"), Body: strings.NewReader("x")})
	if apiCode(err) != "KeyConflict" {
		t.Errorf("child of a file: %v", err)
	}
}

func TestAuthFailures(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	bad := s3.New(e.client.Options(), func(o *s3.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider(accessKey, "wrong", "")
	})
	if _, err := bad.ListBuckets(ctx, &s3.ListBucketsInput{}); apiCode(err) != "SignatureDoesNotMatch" {
		t.Errorf("wrong secret: %v", err)
	}
	unknown := s3.New(e.client.Options(), func(o *s3.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider("nobody", secretKey, "")
	})
	if _, err := unknown.ListBuckets(ctx, &s3.ListBucketsInput{}); apiCode(err) != "InvalidAccessKeyId" {
		t.Errorf("unknown key: %v", err)
	}
}

func (e *env) as(ak, sk string) *s3.Client {
	return s3.New(e.client.Options(), func(o *s3.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider(ak, sk, "")
	})
}

func TestScopedKeysAndPublicRead(t *testing.T) {
	e := setupWith(t, false, func(o *s3api.Options) {
		o.Credentials = append(o.Credentials,
			s3api.Credential{AccessKey: "scoped", SecretKey: "scoped-secret/+=", Buckets: []string{"site"}},
			s3api.Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true})
		o.PublicBuckets = []string{"site"}
	})
	ctx := context.Background()
	mustCreateBucket(t, e, "site")
	mustCreateBucket(t, e, "private")

	scoped := e.as("scoped", "scoped-secret/+=")
	if _, err := scoped.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("site"), Key: aws.String("index.html"),
		Body: strings.NewReader("<h1>hi</h1>"), ContentType: aws.String("text/html")}); err != nil {
		t.Fatalf("scoped put: %v", err)
	}
	if _, err := scoped.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("private"), Key: aws.String("x"), Body: strings.NewReader("x")}); apiCode(err) != "AccessDenied" {
		t.Errorf("scoped put outside its bucket: %v", err)
	}
	out, err := scoped.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "site" {
		t.Errorf("scoped ListBuckets: %+v %v", out, err)
	}

	reader := e.as("reader", "reader-secret")
	if _, err := reader.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("site"), Key: aws.String("index.html")}); err != nil {
		t.Errorf("read-only get: %v", err)
	}
	if _, err := reader.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("site"), Key: aws.String("index.html")}); apiCode(err) != "AccessDenied" {
		t.Errorf("read-only delete: %v", err)
	}

	// A browser fetches the public bucket with no credentials at all.
	resp, err := e.http.Get(e.url + "/site/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "<h1>hi</h1>" || resp.Header.Get("Content-Type") != "text/html" {
		t.Errorf("anonymous GET: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, u := range []string{"/site", "/private/x", "/"} {
		resp, err := e.http.Get(e.url + u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("anonymous GET %s = %d, want 403", u, resp.StatusCode)
		}
	}
}

// TestRunnerCacheFlow replays the GitLab Runner distributed cache: the runner manager
// presigns a GET and a PUT URL offline, and the job's cache helper uploads with PUT,
// probes with HEAD on the GET URL, and downloads with GET.
func TestRunnerCacheFlow(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "runner-cache")
		key := "runner/abc123/project/42/default-protected"
		ps := s3.NewPresignClient(e.client)
		put, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("runner-cache"), Key: aws.String(key)}, s3.WithPresignExpires(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		get, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("runner-cache"), Key: aws.String(key)}, s3.WithPresignExpires(time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		resp, err := e.http.Head(get.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("HEAD before upload: %d", resp.StatusCode)
		}

		archive := bytes.Repeat([]byte("PK\x03\x04cache"), 50000)
		req, _ := http.NewRequest(http.MethodPut, put.URL, bytes.NewReader(archive))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err = e.http.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("cache upload: %v %v", resp, err)
		}
		_ = resp.Body.Close()

		resp, err = e.http.Head(get.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(archive)) || resp.Header.Get("ETag") != `"`+md5hex(archive)+`"` {
			t.Errorf("HEAD on the GET URL: %d %d %s", resp.StatusCode, resp.ContentLength, resp.Header.Get("ETag"))
		}

		resp, err = e.http.Get(get.URL)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !bytes.Equal(got, archive) {
			t.Errorf("cache download differs: %d bytes", len(got))
		}
		if onDisk, _ := os.ReadFile(filepath.Join(e.root, "runner-cache", filepath.FromSlash(key))); !bytes.Equal(onDisk, archive) {
			t.Error("cache archive is not a plain file")
		}
	})
}
