package compat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// TestConditionalDelete sends the conditional delete fields as the SDK serializes them:
// HTTP dates, in the header and in the DeleteObjects document.
func TestConditionalDelete(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "cond-del")
		bkt := aws.String("cond-del")
		put := func(key string) *s3.HeadObjectOutput {
			t.Helper()
			if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: bkt, Key: aws.String(key), Body: strings.NewReader("hello")}); err != nil {
				t.Fatal(err)
			}
			head, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bkt, Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}
			return head
		}
		a := put("a")
		_, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bkt, Key: aws.String("a"), IfMatch: aws.String(`"other"`)})
		var api smithy.APIError
		if !errors.As(err, &api) || api.ErrorCode() != "PreconditionFailed" {
			t.Fatalf("mismatched ETag: %v", err)
		}
		if _, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: bkt, Key: aws.String("a"), IfMatch: a.ETag, IfMatchSize: a.ContentLength, IfMatchLastModifiedTime: a.LastModified,
		}); err != nil {
			t.Fatalf("matching delete: %v", err)
		}

		b := put("b")
		out, err := e.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bkt, Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("b"), ETag: b.ETag, Size: b.ContentLength, LastModifiedTime: b.LastModified},
			{Key: aws.String("c"), ETag: aws.String(`"other"`)},
		}}})
		if err != nil || len(out.Errors) != 0 || len(out.Deleted) != 2 {
			t.Fatalf("DeleteObjects: %+v %v", out, err)
		}
		if _, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bkt, Key: aws.String("b")}); err == nil {
			t.Error("b not deleted")
		}
	})
}
