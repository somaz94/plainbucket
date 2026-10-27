package compat

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestObjectTagging(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "tags")
		bkt := aws.String("tags")
		tagsOf := func(key string) string {
			t.Helper()
			out, err := e.client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: bkt, Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}
			var pairs []string
			for _, tag := range out.TagSet {
				pairs = append(pairs, aws.ToString(tag.Key)+"="+aws.ToString(tag.Value))
			}
			return strings.Join(pairs, "&")
		}
		if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: bkt, Key: aws.String("k"), Body: strings.NewReader("x"), Tagging: aws.String("a=1&b=two words")}); err != nil {
			t.Fatal(err)
		}
		if got := tagsOf("k"); got != "a=1&b=two words" {
			t.Errorf("PutObject tags: %s", got)
		}
		head, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bkt, Key: aws.String("k")})
		if err != nil || aws.ToInt32(head.TagCount) != 2 {
			t.Errorf("TagCount: %v %v", head.TagCount, err)
		}

		if _, err := e.client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: bkt, Key: aws.String("k"), Tagging: &types.Tagging{
			TagSet: []types.Tag{{Key: aws.String("c"), Value: aws.String("3")}},
		}}); err != nil {
			t.Fatalf("PutObjectTagging: %v", err)
		}
		if _, err := e.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bkt, Key: aws.String("copy"), CopySource: aws.String("tags/k"),
			TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("d=4")}); err != nil {
			t.Fatal(err)
		}
		if got := tagsOf("k") + "|" + tagsOf("copy"); got != "c=3|d=4" {
			t.Errorf("after PutObjectTagging and CopyObject: %s", got)
		}
		if _, err := e.client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: bkt, Key: aws.String("k")}); err != nil {
			t.Fatal(err)
		}
		if got := tagsOf("k"); got != "" {
			t.Errorf("after DeleteObjectTagging: %s", got)
		}
	})
}

func TestBucketTagging(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		mustCreateBucket(t, e, "btags")
		bkt := aws.String("btags")
		if _, err := e.client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bkt}); apiCode(err) != "NoSuchTagSet" {
			t.Errorf("untagged: %v", err)
		}
		if _, err := e.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("btags-created"), CreateBucketConfiguration: &types.CreateBucketConfiguration{
			Tags: []types.Tag{{Key: aws.String("owner"), Value: aws.String("compat")}},
		}}); err != nil {
			t.Fatal(err)
		}
		if out, err := e.client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("btags-created")}); err != nil || len(out.TagSet) != 1 || *out.TagSet[0].Value != "compat" {
			t.Errorf("tags from CreateBucket: %v %+v", err, out)
		}
		if _, err := e.client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: bkt, Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("team"), Value: aws.String("ci")}, {Key: aws.String("env"), Value: aws.String("dev")},
		}}}); err != nil {
			t.Fatal(err)
		}
		out, err := e.client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bkt})
		if err != nil || len(out.TagSet) != 2 || *out.TagSet[0].Key != "env" || *out.TagSet[1].Value != "ci" {
			t.Errorf("tags: %v %+v", err, out)
		}
		if _, err := e.client.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: bkt}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bkt}); apiCode(err) != "NoSuchTagSet" {
			t.Errorf("after delete: %v", err)
		}
	})
}
