// Package session issues temporary credentials without storing them. A session token
// carries what it grants, signed with the secret of the key that issued it; the
// temporary access key is derived from the token, and its secret from the token and
// the issuing key's secret. Removing or changing that key ends every session it issued.
package session

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims are what a session grants: the issuing key's access, narrowed.
type Claims struct {
	Parent   string    `json:"p"`           // the access key that issued it
	Buckets  []string  `json:"b,omitempty"` // empty: every bucket the parent may use
	ReadOnly bool      `json:"r,omitempty"`
	Name     string    `json:"n,omitempty"` // the RoleSessionName, for the access log
	Expires  time.Time `json:"e"`
}

// KeyPrefix starts every temporary access key, as AWS's do, so tools recognise one.
const KeyPrefix = "ASIA"

// MaxTokenLen bounds the token Parse decodes, before anything is verified.
const MaxTokenLen = 16 << 10

var b64 = base64.RawURLEncoding.Strict()

var (
	// ErrMalformed rejects a token that is not one of ours.
	ErrMalformed = errors.New("the session token is malformed")
	// ErrExpired rejects a session past its expiry.
	ErrExpired = errors.New("the session token has expired")
	// ErrInvalid rejects a token whose issuer is unknown or that its issuer did not sign.
	ErrInvalid = errors.New("the session token is invalid")
)

func mac(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// Credentials are a session's access key, secret key and token.
type Credentials struct {
	AccessKey, SecretKey, Token string
}

func derive(parentSecret, token string) Credentials {
	sum := sha256.Sum256([]byte(token))
	return Credentials{
		AccessKey: KeyPrefix + strings.ToUpper(hex.EncodeToString(sum[:8])),
		SecretKey: base64.RawStdEncoding.EncodeToString(mac([]byte(parentSecret), "plainbucket session secret", token))[:40],
		Token:     token,
	}
}

// Issue signs c with the secret of the issuing key, c.Parent.
func Issue(parentSecret string, c Claims) Credentials {
	payload, _ := json.Marshal(c)
	body := b64.EncodeToString(payload)
	sig := b64.EncodeToString(mac([]byte(parentSecret), "plainbucket session token", body))
	return derive(parentSecret, body+"."+sig)
}

// Parse checks token against the secret of the key that issued it, which secretOf looks
// up, and returns its claims and credentials.
func Parse(token string, secretOf func(accessKey string) (string, bool), now time.Time) (Claims, Credentials, error) {
	var c Claims
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > MaxTokenLen {
		return c, Credentials{}, ErrMalformed
	}
	payload, err := b64.DecodeString(body)
	if err != nil || decodeClaims(payload, &c) != nil || c.Parent == "" || c.Expires.IsZero() {
		return Claims{}, Credentials{}, ErrMalformed
	}
	secret, ok := secretOf(c.Parent)
	if !ok {
		return Claims{}, Credentials{}, ErrInvalid
	}
	got, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac([]byte(secret), "plainbucket session token", body)) {
		return Claims{}, Credentials{}, ErrInvalid
	}
	if !now.Before(c.Expires) {
		return Claims{}, Credentials{}, ErrExpired
	}
	return c, derive(secret, token), nil
}

func decodeClaims(payload []byte, c *Claims) error {
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	return d.Decode(c)
}
