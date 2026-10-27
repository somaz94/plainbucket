// Package s3api serves the S3 REST API, path-style, over a storage.Store.
package s3api

import (
	"cmp"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/somaz94/plainbucket/internal/console"
	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/ratelimit"
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
	// BucketMetrics reports every bucket's usage in /metrics, not only that of buckets with limits.
	BucketMetrics bool
	// AccessLog logs every request at info level.
	AccessLog bool
	// BodyTimeout is how long an upload may go without delivering minProgress bytes
	// before the connection is cut; 0 disables it.
	BodyTimeout time.Duration
	// Console serves the browser console under /_console/ on path-style hosts.
	Console bool
	// Notifier receives the object events; nil sends none.
	Notifier *notify.Notifier
	// ReadOnly refuses every write with 503; SetReadOnly changes it while serving.
	ReadOnly bool
	// AnonymousRateLimit caps unsigned requests per client address.
	AnonymousRateLimit ratelimit.Limit
}

// Handler is the S3 API http.Handler.
type Handler struct {
	opts      Options
	store     *storage.Store
	log       *slog.Logger
	now       func() time.Time
	metrics   *metrics
	access    atomic.Pointer[access] // swapped whole by SetAccess while serving
	readOnly  atomic.Bool
	writes    atomic.Int64                  // requests in flight that may write
	unready   atomic.Bool                   // the last /readyz failed, so the next success is logged
	rootMoved atomic.Bool                   // the last /readyz found storage.ErrRootMoved
	anonymous atomic.Pointer[ratelimit.Set] // nil without an anonymous rate limit
}

// access is the keys and bucket policies in force.
type access struct {
	creds    map[string]*Credential
	limiters map[string]*ratelimit.Limiter // of the keys with a rate limit
	buckets  map[string]*BucketPolicy
}

// New returns a Handler.
func New(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{opts: opts, store: opts.Store, log: logger, now: time.Now, metrics: newMetrics()}
	h.SetAccess(opts.Credentials, opts.Buckets, opts.PublicBuckets)
	h.readOnly.Store(opts.ReadOnly)
	h.SetAnonymousRateLimit(opts.AnonymousRateLimit)
	return h
}

// Anonymous clients tracked at once, and how long one must be idle to be dropped when full.
const (
	anonClients = 10000
	anonIdle    = 10 * time.Minute
)

// SetAnonymousRateLimit replaces the per-address limit of unsigned requests, starting
// every address afresh; it is safe while serving.
func (h *Handler) SetAnonymousRateLimit(l ratelimit.Limit) {
	if l.Zero() {
		h.anonymous.Store(nil)
		return
	}
	h.anonymous.Store(ratelimit.NewSet(l, anonIdle, anonClients))
}

// SetReadOnly turns the server-wide read-only mode on or off; it is safe while serving.
func (h *Handler) SetReadOnly(on bool) { h.readOnly.Store(on) }

// SetAccess replaces the access keys and bucket policies; it is safe while serving.
// A request already authenticated finishes with the key it was signed with.
func (h *Handler) SetAccess(creds []Credential, buckets map[string]BucketPolicy, public []string) {
	a := &access{creds: map[string]*Credential{}, limiters: map[string]*ratelimit.Limiter{}, buckets: map[string]*BucketPolicy{}}
	old := h.access.Load()
	for _, c := range creds {
		a.creds[c.AccessKey] = &c
		if c.RateLimit.Zero() {
			continue
		}
		// A key whose limit did not change keeps its limiter, and with it its spent allowance.
		if prev := old; prev != nil && prev.creds[c.AccessKey] != nil && prev.creds[c.AccessKey].RateLimit == c.RateLimit {
			a.limiters[c.AccessKey] = prev.limiters[c.AccessKey]
		} else {
			a.limiters[c.AccessKey] = ratelimit.New(c.RateLimit)
		}
	}
	for name, p := range buckets {
		a.buckets[name] = &p
	}
	for _, b := range public {
		if a.buckets[b] == nil {
			a.buckets[b] = &BucketPolicy{}
		}
		a.buckets[b].Public = true
	}
	h.access.Store(a)
}

// request is one API call. auth and cred are nil for an anonymous one.
type request struct {
	w        http.ResponseWriter
	r        *http.Request
	id       string
	auth     *sigv4.Auth
	cred     *Credential
	vhost    bool   // the bucket came from the Host header
	readOnly bool   // the server-wide mode, read once per request
	website  bool   // an unsigned read of a website bucket
	op       string // the operation, when only the body names it
	bucket   string
	key      string
	q        url.Values
}

// Subresources this server does not implement; requests naming one get 501 (bar those
// readableSubresource answers) instead of being mistaken for a plain operation.
var unsupported = map[string]bool{
	"accelerate": true, "acl": true, "analytics": true, "attributes": true, "cors": true,
	"encryption": true, "intelligent-tiering": true, "inventory": true, "legal-hold": true,
	"lifecycle": true, "logging": true, "metrics": true, "notification": true,
	"object-lock": true, "ownershipControls": true, "policy": true, "policyStatus": true,
	"publicAccessBlock": true, "replication": true, "requestPayment": true, "restore": true,
	"retention": true, "select": true, "session": true, "tagging": true, "torrent": true,
	"versions": true, "website": true,
}

// readableSubresource reports the requests for an otherwise unsupported subresource
// that are answered: an object's tags, its attributes, and a bucket's versions, which
// are its objects as the "null" version. Bucket tags stay unsupported.
func readableSubresource(k string, req *request) bool {
	m := req.r.Method
	switch k {
	case "tagging":
		return req.key != "" && (m == http.MethodGet || m == http.MethodPut || m == http.MethodDelete)
	case "attributes":
		return req.key != "" && m == http.MethodGet
	case "versions":
		return req.key == "" && m == http.MethodGet
	}
	return false
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	w.Header().Set("X-Amz-Request-Id", id)
	w.Header().Set("Server", "plainbucket")
	// An S3 request to a bucket named healthz, readyz or metrics is signed, so it never lands here.
	// On a virtual host these paths are object keys (a public bucket serves them unsigned).
	vbucket, vhost := h.vhostBucket(r.Host)
	if !vhost && r.URL.RawQuery == "" && r.Header.Get("Authorization") == "" && r.Method == http.MethodGet {
		switch {
		case r.URL.Path == "/healthz":
			h.serveHealth(w)
			return
		case r.URL.Path == "/readyz":
			h.serveReady(w)
			return
		case r.URL.Path == "/metrics" && h.opts.Metrics:
			h.metrics.serve(w, h.store, h.opts.Notifier, h.readOnly.Load(), h.opts.BucketMetrics, h.writes.Load())
			return
		}
	}

	rec := httpx.NewRecorder(w)
	start := time.Now()
	req := &request{w: rec, r: r, id: id, vhost: vhost, bucket: vbucket, readOnly: h.readOnly.Load()}
	// Counted so an operator turning read-only on can see when the last write is done.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.writes.Add(1)
		defer h.writes.Add(-1)
	}
	if vhost {
		req.key = strings.TrimPrefix(r.URL.Path, "/")
	} else {
		req.bucket, req.key, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	}
	var err error
	op := "Console"
	if h.opts.Console && !vhost && isConsolePath(r.URL.Path) {
		req.bucket, req.key = "", ""
		err = h.serveConsole(req)
	} else {
		op = operation(r, req.bucket, req.key)
		if h.opts.Console && !vhost {
			// A page served from the console's origin could otherwise script a console window.
			rec.Header().Set("Content-Security-Policy", console.ObjectSandbox)
		}
		var done bool
		if done, err = h.applyCORS(req); !done {
			err = h.serve(req)
		}
	}
	if err != nil {
		h.fail(req, err)
	}
	if req.website {
		// An anonymous website request for a folder is served its index document.
		op = map[bool]string{true: "HeadObject", false: "GetObject"}[r.Method == http.MethodHead]
	}
	op = cmp.Or(req.op, op)
	h.metrics.record(op, httpx.MetricMethod(r.Method), rec.Status, rec.Bytes, time.Since(start))
	if h.opts.AccessLog {
		accessKey := ""
		if req.cred != nil {
			accessKey = req.cred.AccessKey
		}
		attrs := []slog.Attr{
			slog.String("operation", op), slog.String("method", r.Method), slog.String("host", r.Host), slog.String("path", r.URL.Path),
			slog.String("query", redactQuery(r.URL.RawQuery)), slog.String("bucket", req.bucket), slog.String("key", req.key),
			slog.Int("status", rec.Status), slog.Int64("bytes", rec.Bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.String("remote", r.RemoteAddr),
			slog.String("access_key", accessKey), slog.String("user_agent", r.UserAgent()), slog.String("request_id", id),
		}
		if req.cred != nil && req.cred.parent != "" {
			attrs = append(attrs, slog.String("issuer", req.cred.parent), slog.String("session", req.cred.session))
		}
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
	}
}

// publish sends the event name for key, changed by req, to the bucket's webhooks.
func (h *Handler) publish(req *request, name, key string, info storage.ObjectInfo) {
	if h.opts.Notifier == nil {
		return
	}
	e := notify.Event{Name: name, Bucket: req.bucket, Key: key, Size: info.Size, ETag: info.ETag, RequestID: req.id, Time: h.now()}
	if req.cred != nil {
		e.Principal = req.cred.AccessKey
	}
	if host, _, err := net.SplitHostPort(req.r.RemoteAddr); err == nil {
		e.SourceIP = host
	}
	h.opts.Notifier.Publish(e)
}

func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func (h *Handler) serve(req *request) error {
	req.q = req.r.URL.Query()
	if isSTS(req.r, req.bucket) {
		h.serveSTS(req)
		return nil
	}
	if req.bucket != "" && isPostUpload(req.r, req.key, req.q) {
		return h.postObject(req)
	}
	if anonymous(req.r, req.q) {
		h.websiteKey(req)
		if err := h.authorizeAnonymous(req); err != nil {
			return err
		}
		if err := h.throttle(req, h.anonLimiter(req), true); err != nil {
			return err
		}
	} else {
		if err := h.verifySigned(req); err != nil {
			return err
		}
		if err := req.authorize(); err != nil {
			return err
		}
		if err := h.throttle(req, h.keyLimiter(req), false); err != nil {
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
		if q.Has("tagging") {
			return h.putObjectTagging(req)
		}
		if q.Has("uploadId") || q.Has("partNumber") {
			return h.uploadPart(req)
		}
		if req.r.Header.Get("X-Amz-Copy-Source") != "" {
			return h.copyObject(req)
		}
		return h.putObject(req)
	case http.MethodDelete:
		if q.Has("tagging") {
			return h.deleteObjectTagging(req)
		}
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
	storage.ErrNoSuchTrash:        s3err.NoSuchKey,
	storage.ErrInvalidPart:        s3err.InvalidPart,
	storage.ErrInvalidChecksum: func() *s3err.Error {
		return s3err.InvalidRequest("The checksum does not match the algorithm or type of the upload.")
	},
	storage.ErrBadDigest: func() *s3err.Error {
		return s3err.BadDigest("The checksum you specified did not match the calculated checksum.")
	},
	storage.ErrInvalidPartOrder:    s3err.InvalidPartOrder,
	storage.ErrEntityTooSmall:      s3err.EntityTooSmall,
	storage.ErrEntityTooLarge:      s3err.EntityTooLarge,
	storage.ErrQuotaExceeded:       s3err.QuotaExceeded,
	storage.ErrInsufficientStorage: s3err.InsufficientStorage,
	syscall.ENOSPC:                 s3err.InsufficientStorage,
	syscall.EDQUOT:                 s3err.InsufficientStorage,
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
	} else if e.Status == http.StatusInsufficientStorage {
		h.log.Warn("write refused: not enough disk space", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id, "err", err)
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
