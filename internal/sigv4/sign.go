package sigv4

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

// Signer signs outgoing requests, for the server's own S3 client.
type Signer struct {
	AccessKey, SecretKey, Region string
	Service                      string // empty: s3
}

// signedAlways are signed when present besides host and every x-amz-* header.
var signedAlways = []string{"content-md5", "content-type", "range"}

// Sign sets X-Amz-Date, X-Amz-Content-Sha256 (payload, the hex SHA-256 of the body or
// UnsignedPayload) and the Authorization header of r. It rewrites r.URL's path and query
// in the encoding it signs, so what is sent is what was signed.
func (s Signer) Sign(r *http.Request, payload string, now time.Time) {
	service := s.Service
	if service == "" {
		service = "s3"
	}
	amzDate := now.UTC().Format(amzDateLayout)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payload)
	path := uriEncode(r.URL.Path, false)
	r.URL.RawPath = path

	names := []string{"host"}
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") || slices.Contains(signedAlways, lk) {
			names = append(names, lk)
		}
	}
	slices.Sort(names)
	signed := strings.Join(names, ";")
	query := canonicalQuery(r.URL.Query(), "")
	r.URL.RawQuery = query
	canonical := r.Method + "\n" + path + "\n" + query + "\n" +
		canonicalHeaders(r, signed) + "\n" + signed + "\n" + payload

	date := amzDate[:8]
	a := Auth{amzDate: amzDate, scope: date + "/" + s.Region + "/" + service + "/aws4_request",
		key: signingKey(s.SecretKey, date, s.Region, service)}
	r.Header.Set("Authorization", Algorithm+" Credential="+s.AccessKey+"/"+a.scope+", SignedHeaders="+signed+", Signature="+a.sign(canonical))
}
