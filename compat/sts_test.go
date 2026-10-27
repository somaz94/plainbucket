package compat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// TestAssumeRole issues a session with the SDK's STS client, then uses it as any client
// would, read-only and limited to one bucket by its session policy.
func TestAssumeRole(t *testing.T) {
	forEachTransport(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		for _, b := range []string{"granted", "other"} {
			if _, err := e.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b)}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("k"), Body: strings.NewReader("data")}); err != nil {
				t.Fatal(err)
			}
		}
		stsClient := sts.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""), HTTPClient: e.http,
		}, func(o *sts.Options) { o.BaseEndpoint = aws.String(e.url) })
		out, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn:         aws.String("arn:aws:iam::000000000000:role/ci"),
			RoleSessionName: aws.String("compat"),
			DurationSeconds: aws.Int32(900),
			Policy:          aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::granted","arn:aws:s3:::granted/*"]}]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		c := out.Credentials
		if !strings.HasPrefix(*c.AccessKeyId, "ASIA") || c.Expiration.Sub(time.Now()) > 15*time.Minute || c.Expiration.Sub(time.Now()) < 14*time.Minute {
			t.Errorf("credentials: %s until %v", *c.AccessKeyId, c.Expiration)
		}

		temp := s3.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(*c.AccessKeyId, *c.SecretAccessKey, *c.SessionToken), HTTPClient: e.http,
		}, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(e.url)
			o.UsePathStyle = true
		})
		got, err := temp.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("granted"), Key: aws.String("k")})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(got.Body)
		_ = got.Body.Close()
		if string(body) != "data" {
			t.Errorf("read %q", body)
		}
		if _, err := temp.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("granted")}); err != nil {
			t.Errorf("list: %v", err)
		}
		if _, err := temp.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("granted"), Key: aws.String("new"), Body: strings.NewReader("x")}); apiCode(err) != "AccessDenied" {
			t.Errorf("write with a read-only session: %v", err)
		}
		if _, err := temp.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("other"), Key: aws.String("k")}); apiCode(err) != "AccessDenied" {
			t.Errorf("read outside the session policy: %v", err)
		}

		// A presigned URL carries the session token in its query.
		req, err := s3.NewPresignClient(temp).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("granted"), Key: aws.String("k")})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(req.URL, "X-Amz-Security-Token=") {
			t.Fatalf("presigned URL without the token: %s", req.URL)
		}
		resp, err := e.http.Get(req.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("presigned GET: %d", resp.StatusCode)
		}

		// A temporary key cannot issue another.
		chained := sts.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(*c.AccessKeyId, *c.SecretAccessKey, *c.SessionToken), HTTPClient: e.http,
		}, func(o *sts.Options) { o.BaseEndpoint = aws.String(e.url) })
		if _, err := chained.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/ci"), RoleSessionName: aws.String("again")}); apiCode(err) != "AccessDenied" {
			t.Errorf("chained AssumeRole: %v", err)
		}
	})
}
