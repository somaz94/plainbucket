// Package sigv4test signs requests for tests. It is written independently of the
// verifier in package sigv4 so the two cannot share a bug.
package sigv4test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Signer holds test credentials.
type Signer struct {
	AccessKey, SecretKey, Region string
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func (s Signer) key(date string) []byte {
	k := hmacSHA256([]byte("AWS4"+s.SecretKey), date)
	k = hmacSHA256(k, s.Region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

func encode(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

// Sign sets X-Amz-Date, X-Amz-Content-Sha256 and an Authorization header covering host
// and every x-amz-* header, and returns the signature.
func (s Signer) Sign(r *http.Request, payload string, at time.Time) string {
	amzDate := at.UTC().Format("20060102T150405Z")
	scope := amzDate[:8] + "/" + s.Region + "/s3/aws4_request"
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payload)

	names := []string{"host"}
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	var pairs []string
	for k, vs := range r.URL.Query() {
		for _, v := range vs {
			pairs = append(pairs, encode(k)+"="+encode(v))
		}
	}
	sort.Strings(pairs)

	var canon strings.Builder
	canon.WriteString(r.Method + "\n" + r.URL.EscapedPath() + "\n" + strings.Join(pairs, "&") + "\n")
	for _, n := range names {
		v := r.Host
		if n != "host" {
			v = strings.Join(r.Header.Values(n), ",")
		}
		canon.WriteString(n + ":" + v + "\n")
	}
	canon.WriteString("\n" + strings.Join(names, ";") + "\n" + payload)
	sum := sha256.Sum256([]byte(canon.String()))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(hmacSHA256(s.key(amzDate[:8]), sts))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, scope, strings.Join(names, ";"), sig))
	return sig
}

// ChunkSignature returns the signature of one aws-chunked chunk.
func (s Signer) ChunkSignature(at time.Time, prev string, chunk []byte) string {
	amzDate := at.UTC().Format("20060102T150405Z")
	scope := amzDate[:8] + "/" + s.Region + "/s3/aws4_request"
	empty := sha256.Sum256(nil)
	h := sha256.Sum256(chunk)
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + amzDate + "\n" + scope + "\n" + prev + "\n" +
		hex.EncodeToString(empty[:]) + "\n" + hex.EncodeToString(h[:])
	return hex.EncodeToString(hmacSHA256(s.key(amzDate[:8]), sts))
}

// PostFields returns the form fields that sign a browser POST upload of the base64
// policy at a given time, names lowercased as the server compares them.
func (s Signer) PostFields(policy string, at time.Time) map[string]string {
	amzDate := at.UTC().Format("20060102T150405Z")
	return map[string]string{
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-credential": s.AccessKey + "/" + amzDate[:8] + "/" + s.Region + "/s3/aws4_request",
		"x-amz-date":       amzDate,
		"x-amz-signature":  hex.EncodeToString(hmacSHA256(s.key(amzDate[:8]), policy)),
		"policy":           policy,
	}
}
