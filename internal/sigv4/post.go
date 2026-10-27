package sigv4

import (
	"encoding/hex"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
)

// VerifyPost checks the signature of a browser POST upload, which signs the base64 policy
// document instead of the request. fields are the form fields, names lowercased. The policy
// itself, with its expiration, is the caller's to check.
func VerifyPost(fields map[string]string, creds Credentials) (*Auth, *s3err.Error) {
	// 403, not S3's 400: s3-tests' Signature Version 2 POST cases expect an unauthenticated refusal.
	if _, ok := fields["awsaccesskeyid"]; ok {
		return nil, s3err.AccessDenied("Signature Version 2 is not supported. Please use AWS4-HMAC-SHA256.")
	}
	if fields["x-amz-algorithm"] != Algorithm {
		return nil, s3err.InvalidArgument("x-amz-algorithm must be AWS4-HMAC-SHA256.")
	}
	cred, ok := parseCredential(fields["x-amz-credential"], "s3")
	if !ok {
		return nil, s3err.InvalidArgument("x-amz-credential is malformed.")
	}
	amzDate := fields["x-amz-date"]
	if _, err := time.Parse(amzDateLayout, amzDate); err != nil || cred.date != amzDate[:8] {
		return nil, s3err.InvalidArgument("x-amz-date is malformed.")
	}
	if fields["policy"] == "" || fields["x-amz-signature"] == "" {
		return nil, s3err.InvalidArgument("A signed POST upload needs the policy and x-amz-signature fields.")
	}
	secret, e := lookupSecret(creds, cred.accessKey)
	if e != nil {
		return nil, e
	}
	want := hex.EncodeToString(hmacSHA256(signingKey(secret, cred.date, cred.region, cred.service), fields["policy"]))
	if !constantTimeEqual(want, fields["x-amz-signature"]) {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return &Auth{AccessKey: cred.accessKey, PayloadHash: UnsignedPayload}, nil
}
