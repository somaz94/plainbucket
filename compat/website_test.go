package compat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/somaz94/plainbucket/internal/s3api"
)

// TestStaticWebsite publishes a site with the SDK and reads it the way a browser would:
// unsigned, by directory URL, from another origin.
func TestStaticWebsite(t *testing.T) {
	e := setupWith(t, false, func(o *s3api.Options) {
		o.Buckets = map[string]s3api.BucketPolicy{"site": {
			Public:  true,
			Website: &s3api.Website{Index: "index.html", Error: "404.html"},
			CORS:    &s3api.CORS{AllowedOrigins: []string{"https://app.example.com"}, MaxAge: time.Hour},
		}}
	})
	ctx := context.Background()
	mustCreateBucket(t, e, "site")
	for key, body := range map[string]string{"index.html": "home", "blog/index.html": "blog", "404.html": "gone"} {
		if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("site"), Key: aws.String(key),
			Body: strings.NewReader(body), ContentType: aws.String("text/html")}); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(method, path string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.url+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b)
	}
	for path, want := range map[string]string{"/site/": "home", "/site/blog/": "blog", "/site/blog": "blog"} {
		if resp, body := fetch(http.MethodGet, path, nil); resp.StatusCode != http.StatusOK || body != want {
			t.Errorf("GET %s = %d %q, want %q", path, resp.StatusCode, body, want)
		}
	}
	if resp, body := fetch(http.MethodGet, "/site/nope", nil); resp.StatusCode != http.StatusNotFound || body != "gone" {
		t.Errorf("error document: %d %q", resp.StatusCode, body)
	}
	resp, _ := fetch(http.MethodOptions, "/site/index.html", map[string]string{"Origin": "https://app.example.com", "Access-Control-Request-Method": "GET"})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = fetch(http.MethodGet, "/site/index.html", map[string]string{"Origin": "https://app.example.com"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("CORS GET: %v", resp.Header)
	}
	// The SDK still sees S3, not the website.
	if _, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("site"), Key: aws.String("nope")}); apiCode(err) != "NoSuchKey" {
		t.Errorf("signed GET of a missing key: %v", err)
	}
}
