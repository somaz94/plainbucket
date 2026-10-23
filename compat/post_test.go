package compat

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// postForm builds the form a browser would send for a presigned POST, the file last.
func postForm(t *testing.T, values map[string]string, extra map[string]string, file string) (io.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, m := range []map[string]string{values, extra} {
		for k, v := range m {
			if err := w.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	fw, err := w.CreateFormFile("file", "report.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(file))
	_ = w.Close()
	return &b, w.FormDataContentType()
}

func TestPresignedPost(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "forms")
		post, err := s3.NewPresignClient(e.client).PresignPostObject(ctx,
			&s3.PutObjectInput{Bucket: aws.String("forms"), Key: aws.String("uploads/report.csv")},
			func(o *s3.PresignPostOptions) {
				o.Expires = time.Minute
				o.Conditions = []any{[]any{"content-length-range", 1, 16}, []any{"starts-with", "$Content-Type", "text/"}}
			})
		if err != nil {
			t.Fatalf("PresignPostObject: %v", err)
		}
		send := func(values map[string]string, contentType, file string) *http.Response {
			t.Helper()
			body, ct := postForm(t, values, map[string]string{"Content-Type": contentType}, file)
			resp, err := e.http.Post(post.URL, ct, body)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp
		}

		if resp := send(post.Values, "text/csv", "a,b\n1,2\n"); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("presigned POST: %s", resp.Status)
		}
		got, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("forms"), Key: aws.String("uploads/report.csv")})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		data, _ := io.ReadAll(got.Body)
		_ = got.Body.Close()
		if string(data) != "a,b\n1,2\n" || aws.ToString(got.ContentType) != "text/csv" {
			t.Errorf("stored %q as %s", data, aws.ToString(got.ContentType))
		}

		if resp := send(post.Values, "text/csv", strings.Repeat("x", 17)); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("over the length range: %s", resp.Status)
		}
		if resp := send(post.Values, "application/json", "{}"); resp.StatusCode != http.StatusForbidden {
			t.Errorf("content type outside the policy: %s", resp.Status)
		}
		tampered := map[string]string{}
		for k, v := range post.Values {
			tampered[k] = v
		}
		tampered["key"] = "elsewhere.csv"
		if resp := send(tampered, "text/csv", "x"); resp.StatusCode != http.StatusForbidden {
			t.Errorf("key outside the policy: %s", resp.Status)
		}
	})
}
