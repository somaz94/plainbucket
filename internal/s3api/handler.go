// Package s3api serves the S3 REST API, path-style, over a storage.Store.
package s3api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
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
	Store         *storage.Store
	AccessKey     string
	SecretKey     string
	Region        string
	MaxObjectSize int64        // 0 = unlimited
	Logger        *slog.Logger // nil = slog.Default()
}

// Handler is the S3 API http.Handler.
type Handler struct {
	opts  Options
	store *storage.Store
	log   *slog.Logger
	now   func() time.Time
}

// New returns a Handler.
func New(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{opts: opts, store: opts.Store, log: logger, now: time.Now}
}

// request is one authenticated API call.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	id     string
	auth   *sigv4.Auth
	bucket string
	key    string
	q      url.Values
}

// Subresources this server does not implement; requests naming one get 501 instead of
// being mistaken for a plain object or bucket operation.
var unsupported = map[string]bool{
	"accelerate": true, "acl": true, "analytics": true, "attributes": true, "cors": true,
	"encryption": true, "intelligent-tiering": true, "inventory": true, "legal-hold": true,
	"lifecycle": true, "logging": true, "metrics": true, "notification": true,
	"object-lock": true, "ownershipControls": true, "policy": true, "policyStatus": true,
	"publicAccessBlock": true, "replication": true, "requestPayment": true, "restore": true,
	"retention": true, "select": true, "session": true, "tagging": true, "torrent": true,
	"versions": true, "website": true,
}

func (h *Handler) credentials(accessKey string) (string, bool) {
	if subtle.ConstantTimeCompare([]byte(accessKey), []byte(h.opts.AccessKey)) != 1 {
		return "", false
	}
	return h.opts.SecretKey, true
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	w.Header().Set("X-Amz-Request-Id", id)
	w.Header().Set("Server", "plainbucket")
	if r.URL.Path == "/healthz" && r.URL.RawQuery == "" && r.Header.Get("Authorization") == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
		return
	}

	rec := httpx.NewRecorder(w)
	start := time.Now()
	req := &request{w: rec, r: r, id: id}
	if err := h.serve(req); err != nil {
		h.fail(req, err)
	}
	if h.log.Enabled(r.Context(), slog.LevelDebug) {
		h.log.LogAttrs(r.Context(), slog.LevelDebug, "request",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("query", r.URL.RawQuery),
			slog.Int("status", rec.Status), slog.Int64("bytes", rec.Bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.String("request_id", id))
	}
}

func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func (h *Handler) serve(req *request) error {
	auth, e := sigv4.Verify(req.r, h.credentials, h.now())
	if e != nil {
		return e
	}
	req.auth = auth
	req.bucket, req.key, _ = strings.Cut(strings.TrimPrefix(req.r.URL.Path, "/"), "/")
	req.q = req.r.URL.Query()
	for k := range req.q {
		if unsupported[k] {
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
		if q.Has("uploadId") && req.r.Method == http.MethodGet {
			return h.listParts(req)
		}
		if q.Has("partNumber") {
			return s3err.NotImplemented()
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
	storage.ErrInvalidPartOrder:   s3err.InvalidPartOrder,
	storage.ErrEntityTooSmall:     s3err.EntityTooSmall,
	storage.ErrEntityTooLarge:     s3err.EntityTooLarge,
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
	for target, mk := range storageErrors {
		if errors.Is(err, target) {
			return mk()
		}
	}
	return nil
}

func (h *Handler) fail(req *request, err error) {
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
