// Package sigv4 verifies AWS Signature Version 4 requests: the Authorization header,
// presigned query strings, and aws-chunked streaming payloads.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
)

const (
	Algorithm = "AWS4-HMAC-SHA256"

	UnsignedPayload          = "UNSIGNED-PAYLOAD"
	StreamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	// EmptySHA256 is the hex SHA-256 of an empty payload.
	EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	amzDateLayout = "20060102T150405Z"
	maxPresignTTL = 7 * 24 * time.Hour
	maxClockSkew  = 15 * time.Minute
)

var hexSHA256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Credentials returns the secret key for an access key.
type Credentials func(accessKey string) (secretKey string, ok bool)

// Auth describes a verified request.
type Auth struct {
	AccessKey string
	// PayloadHash is the x-amz-content-sha256 value the signature covers: a hex digest,
	// UNSIGNED-PAYLOAD, or one of the STREAMING-* modes.
	PayloadHash string

	signature string
	key       []byte
	amzDate   string
	scope     string
}

// Streaming reports whether the body is aws-chunked encoded.
func (a *Auth) Streaming() bool {
	return strings.HasPrefix(a.PayloadHash, "STREAMING-")
}

// Verify authenticates r with either the Authorization header or a presigned query.
func Verify(r *http.Request, creds Credentials, now time.Time) (*Auth, *s3err.Error) {
	if h := r.Header.Get("Authorization"); h != "" {
		return verifyHeader(r, h, creds, now)
	}
	q := r.URL.Query()
	switch {
	case q.Has("X-Amz-Algorithm"):
		return verifyQuery(r, q, creds, now)
	case q.Has("AWSAccessKeyId"):
		return nil, s3err.InvalidRequest("Signature Version 2 is not supported. Please use AWS4-HMAC-SHA256.")
	}
	return nil, s3err.AccessDenied("Anonymous access is not allowed.")
}

type credential struct {
	accessKey, date, region, service string
}

func (c credential) scope() string {
	return c.date + "/" + c.region + "/" + c.service + "/aws4_request"
}

func parseCredential(v string) (credential, bool) {
	p := strings.Split(v, "/")
	if len(p) != 5 || p[0] == "" || p[3] != "s3" || p[4] != "aws4_request" {
		return credential{}, false
	}
	return credential{p[0], p[1], p[2], p[3]}, true
}

func lookupSecret(creds Credentials, accessKey string) (string, *s3err.Error) {
	secret, ok := creds(accessKey)
	if !ok {
		return "", s3err.InvalidAccessKeyID()
	}
	return secret, nil
}

func verifyHeader(r *http.Request, header string, creds Credentials, now time.Time) (*Auth, *s3err.Error) {
	rest, ok := strings.CutPrefix(header, Algorithm+" ")
	if !ok {
		return nil, s3err.InvalidRequest("The authorization mechanism you have provided is not supported. Please use AWS4-HMAC-SHA256.")
	}
	fields := map[string]string{}
	for part := range strings.SplitSeq(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		fields[k] = v
	}
	cred, ok := parseCredential(fields["Credential"])
	signedHeaders, signature := fields["SignedHeaders"], fields["Signature"]
	if !ok || signedHeaders == "" || signature == "" {
		return nil, s3err.AuthHeaderMalformed("The authorization header is malformed.")
	}
	secret, e := lookupSecret(creds, cred.accessKey)
	if e != nil {
		return nil, e
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		if t, err := http.ParseTime(r.Header.Get("Date")); err == nil {
			amzDate = t.UTC().Format(amzDateLayout)
		}
	}
	signedAt, err := time.Parse(amzDateLayout, amzDate)
	if err != nil {
		return nil, s3err.AccessDenied("AWS authentication requires a valid Date or x-amz-date header.")
	}
	if cred.date != amzDate[:8] {
		return nil, s3err.AuthHeaderMalformed("The credential date does not match the request date.")
	}
	if now.Sub(signedAt).Abs() > maxClockSkew {
		return nil, s3err.RequestTimeTooSkewed()
	}
	if !slices.Contains(strings.Split(signedHeaders, ";"), "host") {
		return nil, s3err.AuthHeaderMalformed("host must be a signed header.")
	}
	if e := requireSigned(r, signedHeaders); e != nil {
		return nil, e
	}

	payload := r.Header.Get("X-Amz-Content-Sha256")
	switch {
	case payload == "":
		return nil, s3err.InvalidRequest("Missing required header for this request: x-amz-content-sha256.")
	case payload == UnsignedPayload, payload == StreamingSigned, payload == StreamingSignedTrailer,
		payload == StreamingUnsignedTrailer, hexSHA256Re.MatchString(payload):
	default:
		return nil, s3err.InvalidArgument("x-amz-content-sha256 must be UNSIGNED-PAYLOAD, a STREAMING-* mode, or a hex SHA-256 digest.")
	}

	a := &Auth{
		AccessKey:   cred.accessKey,
		PayloadHash: payload,
		signature:   signature,
		key:         signingKey(secret, cred.date, cred.region, cred.service),
		amzDate:     amzDate,
		scope:       cred.scope(),
	}
	if !a.matches(r, r.URL.Query(), "", signedHeaders, payload) {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return a, nil
}

func verifyQuery(r *http.Request, q url.Values, creds Credentials, now time.Time) (*Auth, *s3err.Error) {
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return nil, s3err.AuthQueryMalformed("X-Amz-Algorithm must be AWS4-HMAC-SHA256.")
	}
	cred, ok := parseCredential(q.Get("X-Amz-Credential"))
	if !ok {
		return nil, s3err.AuthQueryMalformed("X-Amz-Credential is malformed.")
	}
	secret, e := lookupSecret(creds, cred.accessKey)
	if e != nil {
		return nil, e
	}

	amzDate := q.Get("X-Amz-Date")
	signedAt, err := time.Parse(amzDateLayout, amzDate)
	if err != nil || cred.date != amzDate[:8] {
		return nil, s3err.AuthQueryMalformed("X-Amz-Date is malformed.")
	}
	expSec, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expSec <= 0 || time.Duration(expSec)*time.Second > maxPresignTTL {
		return nil, s3err.AuthQueryMalformed("X-Amz-Expires must be between 1 and 604800 seconds.")
	}
	if now.Before(signedAt.Add(-maxClockSkew)) {
		return nil, s3err.AccessDenied("Request is not valid yet.")
	}
	if now.After(signedAt.Add(time.Duration(expSec) * time.Second)) {
		return nil, s3err.AccessDenied("Request has expired.")
	}
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	if !slices.Contains(strings.Split(signedHeaders, ";"), "host") {
		return nil, s3err.AuthQueryMalformed("host must be a signed header.")
	}
	if e := requireSigned(r, signedHeaders); e != nil {
		return nil, e
	}

	payload := UnsignedPayload
	if v := q.Get("X-Amz-Content-Sha256"); v != "" {
		payload = v
	}
	a := &Auth{
		AccessKey:   cred.accessKey,
		PayloadHash: payload,
		signature:   q.Get("X-Amz-Signature"),
		key:         signingKey(secret, cred.date, cred.region, cred.service),
		amzDate:     amzDate,
		scope:       cred.scope(),
	}
	if !a.matches(r, q, "X-Amz-Signature", signedHeaders, payload) {
		return nil, s3err.SignatureDoesNotMatch()
	}
	return a, nil
}

// requireSigned rejects x-amz-* headers the signature does not cover. Without it a
// presigned PUT could gain an x-amz-copy-source and copy any object into its key.
func requireSigned(r *http.Request, signedHeaders string) *s3err.Error {
	signed := strings.Split(signedHeaders, ";")
	for name := range r.Header {
		lk := strings.ToLower(name)
		if strings.HasPrefix(lk, "x-amz-") && !slices.Contains(signed, lk) {
			return s3err.AccessDenied("There were headers present in the request which were not signed: " + lk)
		}
	}
	return nil
}

// matches checks the signature against the strictly re-encoded path first, then the path
// as the client sent it, since S3 signers differ on which characters they escape.
func (a *Auth) matches(r *http.Request, q url.Values, skip, signedHeaders, payload string) bool {
	paths := []string{uriEncode(r.URL.Path, false)}
	if raw := r.URL.EscapedPath(); raw != paths[0] {
		paths = append(paths, raw)
	}
	query := canonicalQuery(q, skip)
	headers := canonicalHeaders(r, signedHeaders)
	for _, p := range paths {
		canonical := r.Method + "\n" + p + "\n" + query + "\n" + headers + "\n" + signedHeaders + "\n" + payload
		if hmac.Equal([]byte(a.sign(canonical)), []byte(a.signature)) {
			return true
		}
	}
	return false
}

func (a *Auth) sign(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	sts := Algorithm + "\n" + a.amzDate + "\n" + a.scope + "\n" + hex.EncodeToString(sum[:])
	return hex.EncodeToString(hmacSHA256(a.key, sts))
}

func canonicalQuery(q url.Values, skip string) string {
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		if k == skip {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

func canonicalHeaders(r *http.Request, signedHeaders string) string {
	var b strings.Builder
	for h := range strings.SplitSeq(signedHeaders, ";") {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(headerValue(r, h))
		b.WriteByte('\n')
	}
	return b.String()
}

// headerValue reads a signed header, including the ones net/http moves out of r.Header.
func headerValue(r *http.Request, name string) string {
	switch name {
	case "host":
		return r.Host
	case "content-length":
		if r.Header.Get("Content-Length") == "" && r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10)
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
	}
	vals := r.Header.Values(name)
	trimmed := make([]string, len(vals))
	for i, v := range vals {
		trimmed[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(trimmed, ",")
}

// uriEncode escapes every byte outside the AWS unreserved set; '/' too when encodeSlash.
func uriEncode(s string, encodeSlash bool) string {
	if s == "" && !encodeSlash {
		return "/"
	}
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// URIEncodePath escapes a key the way S3 does for encoding-type=url, keeping '/'.
func URIEncodePath(s string) string {
	if s == "" {
		return ""
	}
	return uriEncode(s, false)
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// constantTimeEqual compares two strings without leaking their common prefix length.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
