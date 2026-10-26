package s3api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/somaz94/plainbucket/internal/s3err"
)

// Credential is one access key and what it may do.
type Credential struct {
	AccessKey string
	SecretKey string
	// Buckets limits the key to these buckets; empty allows every bucket.
	Buckets []string
	// ReadOnly allows only GET and HEAD.
	ReadOnly bool
}

func (c *Credential) allows(bucket string) bool {
	return len(c.Buckets) == 0 || slices.Contains(c.Buckets, bucket)
}

func (h *Handler) credentials(accessKey string) (string, bool) {
	c, ok := h.access.Load().creds[accessKey]
	if !ok {
		return "", false
	}
	return c.SecretKey, true
}

// signatureParams mark a request as signed even when it carries no Authorization
// header, so a malformed presigned URL is reported as such instead of as anonymous.
var signatureParams = []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Signature", "AWSAccessKeyId", "Signature"}

func anonymous(r *http.Request, q map[string][]string) bool {
	if r.Header.Get("Authorization") != "" {
		return false
	}
	for _, p := range signatureParams {
		if _, ok := q[p]; ok {
			return false
		}
	}
	return true
}

// authorizeAnonymous lets an unsigned request read an object in a public bucket and
// nothing else: no listing, no multipart state, no write.
func (h *Handler) authorizeAnonymous(req *request) error {
	denied := s3err.AccessDenied("Anonymous access is not allowed.")
	m := req.r.Method
	if (m != http.MethodGet && m != http.MethodHead) || req.key == "" || !h.policy(req.bucket).Public {
		return denied
	}
	for k := range req.q {
		if k == "uploadId" || k == "partNumber" || k == "tagging" || k == "attributes" {
			return denied
		}
		if strings.HasPrefix(k, "response-") {
			return s3err.InvalidRequest("Request specific response headers cannot be used for anonymous GET requests.")
		}
	}
	return nil
}

// readOnlyRetry is the Retry-After of a write refused in read-only mode, in seconds.
const readOnlyRetry = "60"

// authorize applies the bucket scope and read-only flag of the key that signed req, and
// the server's read-only mode.
func (req *request) authorize() error {
	c := req.cred
	write := req.r.Method != http.MethodGet && req.r.Method != http.MethodHead
	if c.ReadOnly && write {
		return s3err.AccessDenied("This access key is read-only.")
	}
	if req.bucket != "" && !c.allows(req.bucket) {
		return s3err.AccessDenied("This access key may not access the bucket.")
	}
	// After the permanent refusals: a 503 makes clients retry.
	if req.readOnly && write {
		req.w.Header().Set("Retry-After", readOnlyRetry)
		return s3err.ServiceUnavailable("The server is read-only for maintenance; try again later.")
	}
	return nil
}

// mayRead reports whether the caller may read bucket, for a source other than the
// request's own bucket (CopyObject).
func (req *request) mayRead(bucket string) bool {
	return req.cred == nil || req.cred.allows(bucket)
}
