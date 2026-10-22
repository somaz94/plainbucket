// Package s3api serves the S3 REST API, path-style, over a storage.Store.
package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

// Options configure a Handler.
type Options struct {
	Store       *storage.Store
	Credentials []Credential
	// PublicBuckets may be read anonymously, object by object; listing still needs a key.
	// They are merged into Buckets.
	PublicBuckets []string
	Buckets       map[string]BucketPolicy
	// Domains enable virtual-hosted-style requests: Host <bucket>.<domain> names the
	// bucket and the whole path is the key. Others stay path-style.
	Domains       []string
	Region        string
	MaxObjectSize int64        // 0 = unlimited
	Logger        *slog.Logger // nil = slog.Default()
	// Metrics serves Prometheus metrics at an unauthenticated GET /metrics.
	Metrics bool
	// AccessLog logs every request at info level.
	AccessLog bool
	// BodyTimeout is how long an upload may go without delivering minProgress bytes
	// before the connection is cut; 0 disables it.
	BodyTimeout time.Duration
}

// Handler is the S3 API http.Handler.
type Handler struct {
	opts    Options
	store   *storage.Store
	log     *slog.Logger
	now     func() time.Time
	metrics *metrics
	creds   map[string]*Credential
	buckets map[string]*BucketPolicy
}

// New returns a Handler.
func New(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{opts: opts, store: opts.Store, log: logger, now: time.Now, metrics: newMetrics(),
		creds: map[string]*Credential{}, buckets: map[string]*BucketPolicy{}}
	for i := range opts.Credentials {
		h.creds[opts.Credentials[i].AccessKey] = &opts.Credentials[i]
	}
	for name, p := range opts.Buckets {
		h.buckets[name] = &p
	}
	for _, b := range opts.PublicBuckets {
		if h.buckets[b] == nil {
			h.buckets[b] = &BucketPolicy{}
		}
		h.buckets[b].Public = true
	}
	return h
}

// request is one API call. auth and cred are nil for an anonymous one.
type request struct {
	w       http.ResponseWriter
	r       *http.Request
	id      string
	auth    *sigv4.Auth
	cred    *Credential
	vhost   bool // the bucket came from the Host header
	website bool // an unsigned read of a website bucket
	bucket  string
	key     string
	q       url.Values
}

// Subresources this server does not implement; requests naming one get 501 (bar the
// GETs readableSubresource answers) instead of being mistaken for a plain operation.
var unsupported = map[string]bool{
	"accelerate": true, "acl": true, "analytics": true, "attributes": true, "cors": true,
	"encryption": true, "intelligent-tiering": true, "inventory": true, "legal-hold": true,
	"lifecycle": true, "logging": true, "metrics": true, "notification": true,
	"object-lock": true, "ownershipControls": true, "policy": true, "policyStatus": true,
	"publicAccessBlock": true, "replication": true, "requestPayment": true, "restore": true,
	"retention": true, "select": true, "session": true, "tagging": true, "torrent": true,
	"versions": true, "website": true,
}

// readableSubresource reports the GETs of an unsupported subresource that are answered:
// an object's (empty) tags, which tools copying an object read first, its attributes,
// and a bucket's versions, which are its objects as the "null" version.
func readableSubresource(k string, req *request) bool {
	if req.r.Method != http.MethodGet {
		return false
	}
	switch k {
	case "tagging", "attributes":
		return req.key != ""
	case "versions":
		return req.key == ""
	}
	return false
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	w.Header().Set("X-Amz-Request-Id", id)
	w.Header().Set("Server", "plainbucket")
	// An S3 request to a bucket named healthz or metrics is signed, so it never lands here.
	// On a virtual host these paths are object keys (a public bucket serves them unsigned).
	vbucket, vhost := h.vhostBucket(r.Host)
	if !vhost && r.URL.RawQuery == "" && r.Header.Get("Authorization") == "" && r.Method == http.MethodGet {
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
			return
		case r.URL.Path == "/metrics" && h.opts.Metrics:
			h.metrics.serve(w, h.store)
			return
		}
	}

	rec := httpx.NewRecorder(w)
	start := time.Now()
	req := &request{w: rec, r: r, id: id, vhost: vhost, bucket: vbucket}
	if vhost {
		req.key = strings.TrimPrefix(r.URL.Path, "/")
	} else {
		req.bucket, req.key, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	}
	done, err := h.applyCORS(req)
	if !done {
		err = h.serve(req)
	}
	if err != nil {
		h.fail(req, err)
	}
	h.metrics.record(httpx.MetricMethod(r.Method), rec.Status, rec.Bytes, time.Since(start))
	if h.opts.AccessLog {
		accessKey := ""
		if req.cred != nil {
			accessKey = req.cred.AccessKey
		}
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method), slog.String("host", r.Host), slog.String("path", r.URL.Path),
			slog.String("query", redactQuery(r.URL.RawQuery)), slog.String("bucket", req.bucket), slog.String("key", req.key),
			slog.Int("status", rec.Status), slog.Int64("bytes", rec.Bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.String("remote", r.RemoteAddr),
			slog.String("access_key", accessKey), slog.String("user_agent", r.UserAgent()), slog.String("request_id", id))
	}
}

func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func (h *Handler) serve(req *request) error {
	req.q = req.r.URL.Query()
	if anonymous(req.r, req.q) {
		h.websiteKey(req)
		if err := h.authorizeAnonymous(req); err != nil {
			return err
		}
	} else {
		auth, e := sigv4.Verify(req.r, h.credentials, h.now())
		if e != nil {
			return e
		}
		req.auth, req.cred = auth, h.creds[auth.AccessKey]
		if err := req.authorize(); err != nil {
			return err
		}
	}
	for k := range req.q {
		if unsupported[k] && !readableSubresource(k, req) {
			return s3err.NotImplemented()
		}
	}
	if v, ok := req.q["versionId"]; ok && (len(v) != 1 || v[0] != "null") {
		return s3err.NotImplemented()
	}

	switch {
	case req.bucket == "":
		if req.r.Method != http.MethodGet {
			return s3err.MethodNotAllowed()
		}
		return h.listBuckets(req)
	case req.key == "":
		return h.bucketOp(req)
	case req.website:
		return h.websiteNotFound(req, h.objectOp(req))
	default:
		return h.objectOp(req)
	}
}

func (h *Handler) bucketOp(req *request) error {
	q := req.q
	switch req.r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return h.getBucketLocation(req)
		case q.Has("versioning"):
			return h.getBucketVersioning(req)
		case q.Has("uploads"):
			return h.listMultipartUploads(req)
		case q.Has("versions"):
			return h.listObjectVersions(req)
		case q.Get("list-type") == "2":
			return h.listObjects(req, true)
		case q.Has("list-type"):
			return s3err.InvalidArgument("list-type must be 2.")
		default:
			return h.listObjects(req, false)
		}
	case http.MethodHead:
		return h.headBucket(req)
	case http.MethodPut:
		if q.Has("versioning") {
			return s3err.NotImplemented()
		}
		return h.createBucket(req)
	case http.MethodDelete:
		return h.deleteBucket(req)
	case http.MethodPost:
		if q.Has("delete") {
			return h.deleteObjects(req)
		}
	}
	return s3err.MethodNotAllowed()
}

func (h *Handler) objectOp(req *request) error {
	q := req.q
	if q.Has("versioning") {
		return s3err.NotImplemented()
	}
	switch req.r.Method {
	case http.MethodGet, http.MethodHead:
		if q.Has("tagging") {
			return h.getObjectTagging(req)
		}
		if q.Has("attributes") {
			return h.getObjectAttributes(req)
		}
		if q.Has("uploadId") && req.r.Method == http.MethodGet {
			return h.listParts(req)
		}
		return h.getObject(req)
	case http.MethodPut:
		if q.Has("uploadId") || q.Has("partNumber") {
			return h.uploadPart(req)
		}
		if req.r.Header.Get("X-Amz-Copy-Source") != "" {
			return h.copyObject(req)
		}
		return h.putObject(req)
	case http.MethodDelete:
		if q.Has("uploadId") {
			return h.abortMultipartUpload(req)
		}
		return h.deleteObject(req)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return h.createMultipartUpload(req)
		case q.Has("uploadId"):
			return h.completeMultipartUpload(req)
		}
	}
	return s3err.MethodNotAllowed()
}

var storageErrors = map[error]func() *s3err.Error{
	storage.ErrNoSuchBucket:       s3err.NoSuchBucket,
	storage.ErrNoSuchKey:          s3err.NoSuchKey,
	storage.ErrBucketExists:       s3err.BucketAlreadyOwnedByYou,
	storage.ErrBucketNotEmpty:     s3err.BucketNotEmpty,
	storage.ErrInvalidBucketName:  s3err.InvalidBucketName,
	storage.ErrInvalidKey:         s3err.InvalidKey,
	storage.ErrKeyTooLong:         s3err.KeyTooLong,
	storage.ErrConflict:           s3err.KeyConflict,
	storage.ErrShortBody:          s3err.IncompleteBody,
	storage.ErrPreconditionFailed: s3err.PreconditionFailed,
	storage.ErrNoSuchUpload:       s3err.NoSuchUpload,
	storage.ErrInvalidPart:        s3err.InvalidPart,
	storage.ErrInvalidChecksum: func() *s3err.Error {
		return s3err.InvalidRequest("The checksum does not match the algorithm or type of the upload.")
	},
	storage.ErrBadDigest: func() *s3err.Error {
		return s3err.BadDigest("The checksum you specified did not match the calculated checksum.")
	},
	storage.ErrInvalidPartOrder: s3err.InvalidPartOrder,
	storage.ErrEntityTooSmall:   s3err.EntityTooSmall,
	storage.ErrEntityTooLarge:   s3err.EntityTooLarge,
	storage.ErrQuotaExceeded:    s3err.QuotaExceeded,
	storage.ErrMarkerNotEmpty: func() *s3err.Error {
		return s3err.InvalidArgument("An object key ending in / is a directory and must have an empty body.")
	},
	storage.ErrInvalidPartNumber: func() *s3err.Error {
		return s3err.InvalidArgument("Part number must be an integer between 1 and 10000, inclusive.")
	},
}

// toS3 maps err to its S3 error, or nil when it is not a known client error.
func toS3(err error) *s3err.Error {
	var e *s3err.Error
	if errors.As(err, &e) {
		return e
	}
	var re *storage.RangeError
	if errors.As(err, &re) {
		return s3err.InvalidArgument(fmt.Sprintf("Range specified is not valid for source object of size: %d", re.Size))
	}
	for target, mk := range storageErrors {
		if errors.Is(err, target) {
			return mk()
		}
	}
	return nil
}

func (h *Handler) fail(req *request, err error) {
	if errors.Is(err, context.Canceled) && req.r.Context().Err() != nil {
		h.log.Debug("request canceled by the client", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id)
		return
	}
	e := toS3(err)
	if e == nil {
		h.log.Error("request failed", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id, "err", err)
		e = s3err.Internal()
	} else if e.Status == http.StatusForbidden {
		// Debug, not warn: unsigned probes would flood the log.
		h.log.Debug("request denied", "method", req.r.Method, "path", req.r.URL.Path, "code", e.Code, "reason", e.Message)
	}
	e.Write(req.w, req.r, req.id)
}

// vhostBucket returns the bucket a virtual-hosted-style Host names. The longest matching
// domain wins, so s3.example.com and example.com can both be configured.
func (h *Handler) vhostBucket(host string) (string, bool) {
	if len(h.opts.Domains) == 0 {
		return "", false
	}
	if hp, _, err := net.SplitHostPort(host); err == nil {
		host = hp
	}
	host = strings.ToLower(host)
	best := ""
	for _, d := range h.opts.Domains {
		if host == d {
			return "", false // the endpoint itself is path-style
		}
		if len(d) > len(best) && strings.HasSuffix(host, "."+d) {
			best = d
		}
	}
	if best == "" {
		return "", false
	}
	b := strings.TrimSuffix(host, "."+best)
	return b, storage.ValidBucketName(b)
}

// url returns the absolute URL of key in the request's bucket, addressed as the request was.
func (req *request) url(key string) string {
	scheme := "http"
	if req.r.TLS != nil {
		scheme = "https"
	}
	path := "/" + req.bucket + "/" + sigv4.URIEncodePath(key)
	if req.vhost {
		path = "/" + sigv4.URIEncodePath(key)
	}
	return scheme + "://" + req.r.Host + path
}

// redactQuery hides the signature of a presigned URL, which would let anyone reading the
// log replay the request until the URL expires.
func redactQuery(raw string) string {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Security-Token", "Signature"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	return q.Encode()
}
