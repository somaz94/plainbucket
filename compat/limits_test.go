package compat

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/storage"
)

func TestBucketSizeLimit(t *testing.T) {
	e := setupWith(t, false, func(o *s3api.Options) {
		o.Store.SetLimits(map[string]storage.Limit{"capped": {MaxSize: 10}})
	})
	ctx := context.Background()
	mustCreateBucket(t, e, "capped")
	put := func(key, body string) error {
		_, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("capped"), Key: aws.String(key), Body: strings.NewReader(body)})
		return err
	}
	if err := put("a", "12345678"); err != nil {
		t.Fatal(err)
	}
	if err := put("b", "12345"); apiCode(err) != "QuotaExceeded" {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("capped"), Key: aws.String("a")}); err != nil {
		t.Fatal(err)
	}
	if err := put("b", "12345"); err != nil {
		t.Errorf("after freeing space: %v", err)
	}
}
