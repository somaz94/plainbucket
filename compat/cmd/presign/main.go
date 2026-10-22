// Command presign prints a presigned URL made by the AWS SDK for Go v2, as the GitLab
// Runner manager makes them for its cache helper. Credentials come from AWS_ACCESS_KEY_ID
// and AWS_SECRET_ACCESS_KEY.
//
// Usage: presign <endpoint> <GET|PUT|HEAD> <bucket> <key>
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "presign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: presign <endpoint> <GET|PUT|HEAD> <bucket> <key>")
	}
	endpoint, method, bucket, key := args[0], args[1], aws.String(args[2]), aws.String(args[3])
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
	})
	ps := s3.NewPresignClient(client, s3.WithPresignExpires(time.Hour))
	ctx := context.Background()
	var (
		req *v4.PresignedHTTPRequest
		err error
	)
	switch method {
	case "GET":
		req, err = ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	case "PUT":
		req, err = ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key})
	case "HEAD":
		req, err = ps.PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	default:
		return fmt.Errorf("unknown method %q", method)
	}
	if err != nil {
		return err
	}
	fmt.Println(req.URL)
	return nil
}
