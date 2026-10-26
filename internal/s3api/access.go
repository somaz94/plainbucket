package s3api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/ratelimit"
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
	// RateLimit caps the key's requests and body bytes; zero does not limit.
	RateLimit ratelimit.Limit
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

// throttle counts req against limiter, refusing it with 503 SlowDown past the request
// rate, and slows its bodies to the byte rate. It runs once the caller is known.
func (h *Handler) throttle(req *request, lm *ratelimit.Limiter, anonymous bool) error {
	if lm == nil {
		return nil
	}
	if ok, wait := lm.Allow(); !ok {
		h.metrics.throttled(anonymous)
		req.w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
		return s3err.SlowDown()
	}
	ctx := req.r.Context()
	req.r.Body = lm.Reader(ctx, req.r.Body)
	req.w = lm.Writer(ctx, req.w)
	return nil
}

// keyLimiter returns the limiter of the key that signed req, nil when it has none.
func (h *Handler) keyLimiter(req *request) *ratelimit.Limiter {
	return h.access.Load().limiters[req.cred.AccessKey]
}

// anonLimiter returns the limiter of an unsigned request's client address.
func (h *Handler) anonLimiter(req *request) *ratelimit.Limiter {
	set := h.anonymous.Load()
	if set == nil {
		return nil
	}
	return set.Get(clientKey(req.r.RemoteAddr))
}

// clientKey names the client of a remote address: an IPv4 address, or the /64 of an IPv6
// one, which a single host can fill with addresses.
func clientKey(remote string) string {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if a = a.Unmap().WithZone(""); a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// mayRead reports whether the caller may read bucket, for a source other than the
// request's own bucket (CopyObject).
func (req *request) mayRead(bucket string) bool {
	return req.cred == nil || req.cred.allows(bucket)
}
