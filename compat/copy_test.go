package compat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestUploadPartCopy assembles an object from ranges of another, as the aws cli and rclone
// do for server-side copies above their multipart threshold.
func TestUploadPartCopy(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "cp-src")
		mustCreateBucket(t, e, "cp-dst")
		data := bytes.Repeat([]byte("0123456789abcdef"), 12<<16) // 12 MiB
		if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("cp-src"), Key: aws.String("big"), Body: bytes.NewReader(data)}); err != nil {
			t.Fatal(err)
		}
		tags, err := e.client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("cp-src"), Key: aws.String("big")})
		if err != nil || len(tags.TagSet) != 0 {
			t.Fatalf("GetObjectTagging: %+v %v", tags, err)
		}
		up, err := e.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("cp-dst"), Key: aws.String("copy")})
		if err != nil {
			t.Fatal(err)
		}
		const partSize = 5 << 20
		var parts []types.CompletedPart
		for i, start := int32(1), 0; start < len(data); i, start = i+1, start+partSize {
			end := min(start+partSize, len(data)) - 1
			out, err := e.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
				Bucket: aws.String("cp-dst"), Key: aws.String("copy"), UploadId: up.UploadId, PartNumber: aws.Int32(i),
				CopySource: aws.String("cp-src/big"), CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
			})
			if err != nil {
				t.Fatalf("UploadPartCopy %d: %v", i, err)
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(i), ETag: out.CopyPartResult.ETag})
		}
		if _, err := e.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("cp-dst"), Key: aws.String("copy"), UploadId: up.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("cp-dst"), Key: aws.String("copy")})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(got.Body)
		_ = got.Body.Close()
		if !bytes.Equal(body, data) {
			t.Errorf("copied %d bytes, want %d", len(body), len(data))
		}
		if onDisk, _ := os.ReadFile(filepath.Join(e.root, "cp-dst", "copy")); !bytes.Equal(onDisk, data) {
			t.Error("copy is not a plain file with the source bytes")
		}
	})
}
