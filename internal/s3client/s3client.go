// Package s3client is the small S3 client the server uses to replicate objects to
// another S3 endpoint: single and multipart uploads, HEAD, DELETE, tagging and
// ListObjectsV2, signed with SigV4 and nothing beyond the standard library.
package s3client

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/sigv4"
)

// Client talks to one S3 endpoint.
type Client struct {
	// Endpoint is the scheme and host, such as https://s3.us-east-1.amazonaws.com.
	Endpoint *url.URL
	// VirtualHosted names the bucket in the host (bucket.endpoint) instead of the path.
	VirtualHosted bool
	Signer        sigv4.Signer
	HTTP          *http.Client
	Now           func() time.Time // nil: time.Now
}

// Error is an S3 error response.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return fmt.Sprintf("s3 %d %s: %s", e.Status, e.Code, e.Message) }

// IsNotFound reports a missing key or bucket.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Tag is one object tag.
type Tag struct{ Key, Value string }

// Meta is what travels with an object besides its data.
type Meta struct {
	ContentType string
	UserMeta    map[string]string // without the x-amz-meta- prefix
	Headers     map[string]string // stored headers such as Cache-Control
	Tags        []Tag
}

// Object is a listed or HEAD-ed object.
type Object struct {
	Key  string
	Size int64
	ETag string // with its quotes, as S3 sends it
	Meta Meta   // HEAD only
}

// Part is a finished part of a multipart upload.
type Part struct {
	Number int
	ETag   string
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) url(bucket, key string, q url.Values) *url.URL {
	u := *c.Endpoint
	if c.VirtualHosted {
		u.Host = bucket + "." + u.Host
		u.Path = "/" + key
	} else {
		u.Path = "/" + bucket + "/" + key
	}
	u.RawQuery = q.Encode()
	return &u
}

// do sends a signed request. A nil body is empty; size is its length, required for a
// body. A body is signed by sum, its hex SHA-256, when the caller has it in memory, and
// otherwise sent as UNSIGNED-PAYLOAD, which over plain http needs md5 to protect the data.
func (c *Client) do(ctx context.Context, method, bucket, key string, q url.Values, h http.Header, body io.Reader, size int64, md5 []byte, sum string) (*http.Response, error) {
	if body != nil && size < 0 {
		return nil, errors.New("s3client: body size unknown")
	}
	if body != nil && size > 0 && md5 == nil && sum == "" && c.Endpoint.Scheme != "https" {
		return nil, errors.New("s3client: a body over plain http needs its MD5")
	}
	r, err := http.NewRequestWithContext(ctx, method, c.url(bucket, key, q).String(), body)
	if err != nil {
		return nil, err
	}
	maps.Copy(r.Header, h)
	payload := sigv4.EmptySHA256
	if body != nil {
		r.ContentLength, payload = size, cmp.Or(sum, sigv4.UnsignedPayload)
		if size == 0 {
			r.Body, payload = http.NoBody, sigv4.EmptySHA256
		}
	}
	if md5 != nil {
		r.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(md5))
	}
	c.Signer.Sign(r, payload, c.now())
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer drain(resp)
		e := &Error{Status: resp.StatusCode}
		if raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); len(raw) > 0 {
			var doc struct{ Code, Message string }
			if xml.Unmarshal(raw, &doc) == nil {
				e.Code, e.Message = doc.Code, doc.Message
			}
		}
		if e.Code == "" {
			e.Code = http.StatusText(resp.StatusCode)
		}
		return nil, e
	}
	return resp, nil
}

// drain reads the rest of the body, so the connection can be reused, and closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func metaHeaders(m Meta) http.Header {
	h := http.Header{}
	if m.ContentType != "" {
		h.Set("Content-Type", m.ContentType)
	}
	for k, v := range m.UserMeta {
		h.Set("X-Amz-Meta-"+k, v)
	}
	for k, v := range m.Headers {
		h.Set(k, v)
	}
	if len(m.Tags) > 0 {
		q := url.Values{}
		for _, t := range m.Tags {
			q.Add(t.Key, t.Value)
		}
		h.Set("X-Amz-Tagging", q.Encode())
	}
	return h
}

// PutObject uploads size bytes of body as bucket/key and returns its ETag. md5, when
// known, lets the endpoint check the data.
func (c *Client) PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64, md5 []byte, m Meta) (string, error) {
	resp, err := c.do(ctx, http.MethodPut, bucket, key, nil, metaHeaders(m), body, size, md5, "")
	if err != nil {
		return "", err
	}
	drain(resp)
	return resp.Header.Get("ETag"), nil
}

// CreateMultipartUpload starts a multipart upload and returns its id.
func (c *Client) CreateMultipartUpload(ctx context.Context, bucket, key string, m Meta) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, bucket, key, url.Values{"uploads": {""}}, metaHeaders(m), nil, 0, nil, "")
	if err != nil {
		return "", err
	}
	defer drain(resp)
	var doc struct{ UploadId string }
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil || doc.UploadId == "" {
		return "", fmt.Errorf("CreateMultipartUpload: no upload id: %v", err)
	}
	return doc.UploadId, nil
}

// UploadPart uploads part n and returns its ETag.
func (c *Client) UploadPart(ctx context.Context, bucket, key, id string, n int, body io.Reader, size int64, md5 []byte) (string, error) {
	q := url.Values{"uploadId": {id}, "partNumber": {strconv.Itoa(n)}}
	resp, err := c.do(ctx, http.MethodPut, bucket, key, q, nil, body, size, md5, "")
	if err != nil {
		return "", err
	}
	drain(resp)
	return resp.Header.Get("ETag"), nil
}

// CompleteMultipartUpload assembles parts and returns the object's ETag.
func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, key, id string, parts []Part) (string, error) {
	type xmlPart struct {
		PartNumber int
		ETag       string
	}
	doc := struct {
		XMLName xml.Name  `xml:"CompleteMultipartUpload"`
		Parts   []xmlPart `xml:"Part"`
	}{}
	for _, p := range parts {
		doc.Parts = append(doc.Parts, xmlPart{p.Number, p.ETag})
	}
	raw, _ := xml.Marshal(doc)
	resp, err := c.do(ctx, http.MethodPost, bucket, key, url.Values{"uploadId": {id}}, http.Header{"Content-Type": {"application/xml"}},
		bytes.NewReader(raw), int64(len(raw)), nil, sha256Hex(raw))
	if err != nil {
		return "", err
	}
	defer drain(resp)
	// A failure after the 200 status line comes back as an Error document in the body,
	// after whitespace S3 sends to keep the connection open while it assembles the parts.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	var res struct {
		XMLName xml.Name
		ETag    string
		Code    string
		Message string
	}
	if err := xml.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("CompleteMultipartUpload: %w", err)
	}
	switch {
	case res.XMLName.Local == "Error":
		return "", &Error{Status: http.StatusOK, Code: res.Code, Message: res.Message}
	case res.XMLName.Local != "CompleteMultipartUploadResult" || res.ETag == "":
		return "", fmt.Errorf("CompleteMultipartUpload: unexpected response <%s>", res.XMLName.Local)
	}
	return res.ETag, nil
}

// AbortMultipartUpload discards an upload.
func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, key, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, key, url.Values{"uploadId": {id}}, nil, nil, 0, nil, "")
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// DeleteObject removes bucket/key; a missing key is not an error. Any other 404, such
// as a proxy's, is: it does not show that the key is gone.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, key, nil, nil, nil, 0, nil, "")
	var e *Error
	if errors.As(err, &e) && e.Code == "NoSuchKey" {
		return nil
	}
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// PutObjectTagging replaces the tags of bucket/key; no tags removes them.
func (c *Client) PutObjectTagging(ctx context.Context, bucket, key string, tags []Tag) error {
	if len(tags) == 0 {
		resp, err := c.do(ctx, http.MethodDelete, bucket, key, url.Values{"tagging": {""}}, nil, nil, 0, nil, "")
		if err != nil {
			return err
		}
		drain(resp)
		return nil
	}
	type xmlTag struct{ Key, Value string }
	doc := struct {
		XMLName xml.Name `xml:"Tagging"`
		Tags    []xmlTag `xml:"TagSet>Tag"`
	}{}
	for _, t := range tags {
		doc.Tags = append(doc.Tags, xmlTag(t))
	}
	raw, _ := xml.Marshal(doc)
	sum := md5.Sum(raw)
	resp, err := c.do(ctx, http.MethodPut, bucket, key, url.Values{"tagging": {""}}, http.Header{"Content-Type": {"application/xml"}},
		bytes.NewReader(raw), int64(len(raw)), sum[:], sha256Hex(raw))
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// GetObjectTagging returns the tags of bucket/key.
func (c *Client) GetObjectTagging(ctx context.Context, bucket, key string) ([]Tag, error) {
	resp, err := c.do(ctx, http.MethodGet, bucket, key, url.Values{"tagging": {""}}, nil, nil, 0, nil, "")
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	var doc struct {
		Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("GetObjectTagging: %w", err)
	}
	tags := make([]Tag, len(doc.Tags))
	for i, t := range doc.Tags {
		tags[i] = Tag(t)
	}
	return tags, nil
}

// HeadObject returns the size, ETag and metadata of bucket/key.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (Object, error) {
	resp, err := c.do(ctx, http.MethodHead, bucket, key, nil, nil, nil, 0, nil, "")
	if err != nil {
		return Object{}, err
	}
	drain(resp)
	o := Object{Key: key, Size: resp.ContentLength, ETag: resp.Header.Get("ETag"),
		Meta: Meta{ContentType: resp.Header.Get("Content-Type"), UserMeta: map[string]string{}, Headers: map[string]string{}}}
	for _, k := range httpx.StoredHeaders {
		if v := resp.Header.Get(k); v != "" {
			o.Meta.Headers[k] = v
		}
	}
	for k, vs := range resp.Header {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && len(vs) > 0 {
			o.Meta.UserMeta[name] = DecodeMeta(vs[0])
		}
	}
	return o, nil
}

var metaDecoder mime.WordDecoder

// DecodeMeta undoes the RFC 2047 encoding AWS S3 returns non-ASCII metadata values in; a
// value that does not decode passes unchanged.
func DecodeMeta(v string) string {
	if d, err := metaDecoder.DecodeHeader(v); err == nil {
		return d
	}
	return v
}

// ListObjects passes every object of bucket under prefix to fn in key order.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string, fn func(Object) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "encoding-type": {"url"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, bucket, "", q, nil, nil, 0, nil, "")
		if err != nil {
			return err
		}
		var page struct {
			Contents []struct {
				Key  string
				Size int64
				ETag string
			}
			IsTruncated           bool
			NextContinuationToken string
			EncodingType          string
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		drain(resp)
		if err != nil {
			return fmt.Errorf("ListObjectsV2: %w", err)
		}
		for _, o := range page.Contents {
			key := o.Key
			// S3 and MinIO encode a space as '+', so this is query decoding, not path decoding.
			if page.EncodingType == "url" {
				if key, err = url.QueryUnescape(o.Key); err != nil {
					return fmt.Errorf("ListObjectsV2: key %q: %w", o.Key, err)
				}
			}
			if err := fn(Object{Key: key, Size: o.Size, ETag: o.ETag}); err != nil {
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		if page.NextContinuationToken == "" || page.NextContinuationToken == token {
			return errors.New("ListObjectsV2: a truncated page without a new continuation token")
		}
		token = page.NextContinuationToken
	}
}

// MD5 returns the digest an S3 ETag holds for a single-part object, or nil when etag is
// a multipart or otherwise not an MD5 ETag. Pass only ETags of unencrypted objects: SSE-KMS
// and SSE-C ETags look the same but are not digests.
func MD5(etag string) []byte {
	raw, err := hex.DecodeString(strings.Trim(etag, `"`))
	if err != nil || len(raw) != 16 {
		return nil
	}
	return raw
}
