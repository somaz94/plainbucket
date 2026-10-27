package s3api

import (
	"cmp"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/ratelimit"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/session"
	"github.com/somaz94/plainbucket/internal/sigv4"
)

// Credential is one access key and what it may do.
type Credential struct {
	AccessKey string
	SecretKey string
	// Buckets limits the key to these buckets; empty allows every bucket.
	Buckets []string
	// ReadOnly allows only GET and HEAD, and AssumeRole for read-only sessions.
	ReadOnly bool
	// RateLimit caps the key's requests and body bytes; zero does not limit.
	RateLimit ratelimit.Limit

	parent  string // the key that issued this temporary one
	session string // its RoleSessionName
}

func (c *Credential) allows(bucket string) bool {
	return len(c.Buckets) == 0 || slices.Contains(c.Buckets, bucket)
}

// secret looks a long-term key up. A request verifies and authorizes against one
// snapshot, so a reload between the two cannot pair an old secret with new access.
func (a *access) secret(accessKey string) (string, bool) {
	if c := a.creds[accessKey]; c != nil {
		return c.SecretKey, true
	}
	return "", false
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

// keyLimiter returns the limiter of the key that signed req, nil when it has none. A
// temporary key shares its issuer's.
func (h *Handler) keyLimiter(req *request) *ratelimit.Limiter {
	return h.access.Load().limiters[cmp.Or(req.cred.parent, req.cred.AccessKey)]
}

// verifySigned authenticates a signed request and sets req.auth and req.cred. A request
// with a session token is signed with the temporary key the token derives, and gets its
// issuer's access narrowed by the token.
func (h *Handler) verifySigned(req *request) error {
	a := h.access.Load()
	creds, temp := sigv4.Credentials(a.secret), (*Credential)(nil)
	if token := cmp.Or(req.r.Header.Get("X-Amz-Security-Token"), req.q.Get("X-Amz-Security-Token")); token != "" {
		var err error
		if temp, err = h.sessionCredential(a, token); err != nil {
			return err
		}
		creds = func(k string) (string, bool) { return temp.SecretKey, k == temp.AccessKey }
	}
	auth, e := sigv4.Verify(req.r, creds, h.now())
	if e != nil {
		return e
	}
	req.auth, req.cred = auth, cmp.Or(temp, a.creds[auth.AccessKey])
	return nil
}

// sessionCredential returns the temporary key of a session token.
func (h *Handler) sessionCredential(a *access, token string) (*Credential, error) {
	claims, sc, err := session.Parse(token, a.secret, h.now())
	switch {
	case errors.Is(err, session.ErrExpired):
		return nil, s3err.ExpiredToken()
	case err != nil:
		return nil, s3err.InvalidToken()
	}
	p := a.creds[claims.Parent]
	return &Credential{AccessKey: sc.AccessKey, SecretKey: sc.SecretKey, Buckets: narrow(p.Buckets, claims.Buckets),
		ReadOnly: p.ReadOnly || claims.ReadOnly, RateLimit: p.RateLimit, parent: p.AccessKey, session: claims.Name}, nil
}

// noBucket is a bucket scope that allows none: no bucket has an empty name.
var noBucket = []string{""}

// narrow returns the buckets both scopes allow; an empty scope allows every bucket.
func narrow(have, want []string) []string {
	switch {
	case len(want) == 0:
		return have
	case len(have) == 0:
		return want
	}
	var both []string
	for _, b := range want {
		if slices.Contains(have, b) {
			both = append(both, b)
		}
	}
	if len(both) == 0 {
		return noBucket
	}
	return both
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
