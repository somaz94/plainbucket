// Package s3api serves the S3 REST API, path-style, over a storage.Store.
package s3api

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/console"
	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/ratelimit"
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
	// Replicator is told of every committed change to an object; nil replicates nothing.
	Replicator Replicator
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
			h.serveMetrics(w)
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

func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
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
