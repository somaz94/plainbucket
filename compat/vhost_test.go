package compat

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/somaz94/plainbucket/internal/s3api"
)

// vhostDomain is covered by the httptest certificate (*.example.com), so TLS verifies.
const vhostDomain = "example.com"

// vhostClient returns a client using virtual-hosted-style addressing whose connections
// to any *.example.com host reach the test server.
func vhostClient(e *env) (*s3.Client, *http.Client) {
	tr := e.http.Transport.(*http.Transport).Clone()
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, e.addr)
	}
	hc := &http.Client{Transport: tr}
	scheme := "http"
	if e.tls {
		scheme = "https"
	}
	_, port, _ := net.SplitHostPort(e.addr)
	client := s3.New(e.client.Options(), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(scheme + "://" + vhostDomain + ":" + port)
		o.UsePathStyle = false
		o.HTTPClient = hc
	})
	return client, hc
}

func TestVirtualHostedStyle(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "http"
		if useTLS {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, useTLS, func(o *s3api.Options) {
				o.Domains = []string{vhostDomain}
				o.PublicBuckets = []string{"vhost"}
			})
			c, hc := vhostClient(e)
			ctx := context.Background()
			if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("vhost")}); err != nil {
				t.Fatalf("CreateBucket: %v", err)
			}
			if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("vhost"), Key: aws.String("dir/a b.txt"), Body: strings.NewReader("hello")}); err != nil {
				t.Fatalf("PutObject: %v", err)
			}
			got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("vhost"), Key: aws.String("dir/a b.txt")})
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			body, _ := io.ReadAll(got.Body)
			_ = got.Body.Close()
			if string(body) != "hello" {
				t.Errorf("body = %q", body)
			}
			list, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("vhost"), Delimiter: aws.String("/")})
			if err != nil || len(list.CommonPrefixes) != 1 || aws.ToString(list.CommonPrefixes[0].Prefix) != "dir/" {
				t.Errorf("ListObjectsV2: %+v %v", list, err)
			}

			data := bytes.Repeat([]byte("v"), 11<<20)
			up := manager.NewUploader(c, func(u *manager.Uploader) { u.PartSize = 5 << 20; u.Concurrency = 2 })
			res, err := up.Upload(ctx, &s3.PutObjectInput{Bucket: aws.String("vhost"), Key: aws.String("big"), Body: bytes.NewReader(data)})
			if err != nil {
				t.Fatalf("multipart upload: %v", err)
			}
			if want := "/big"; !strings.HasSuffix(res.Location, "vhost."+vhostDomain+":"+portOf(e)+want) {
				t.Errorf("Location = %s", res.Location)
			}

			ps := s3.NewPresignClient(c)
			req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("vhost"), Key: aws.String("dir/a b.txt")}, s3.WithPresignExpires(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(req.URL, "//vhost."+vhostDomain) {
				t.Fatalf("presigned URL is not virtual-hosted: %s", req.URL)
			}
			for _, u := range []string{req.URL, strings.SplitN(req.URL, "?", 2)[0]} { // signed, then anonymous
				resp, err := hc.Get(u)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || string(b) != "hello" {
					t.Errorf("GET %s: %d %q", u, resp.StatusCode, b)
				}
			}
		})
	}
}

func portOf(e *env) string {
	_, port, _ := net.SplitHostPort(e.addr)
	return port
}
