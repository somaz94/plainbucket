package s3api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/console"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
)

const consoleUsagePath = console.Prefix + "api/usage"

var consoleFiles = console.Handler()

// isConsolePath reports whether a path-style request belongs to the console.
func isConsolePath(p string) bool {
	return p == strings.TrimSuffix(console.Prefix, "/") || strings.HasPrefix(p, console.Prefix)
}

// serveConsole answers the console's files to anyone and its usage API to signed
// requests; everything the console does to buckets goes through the S3 API.
func (h *Handler) serveConsole(req *request) error {
	switch p := req.r.URL.Path; {
	case p == strings.TrimSuffix(console.Prefix, "/"):
		console.SetSecurityHeaders(req.w.Header())
		if req.r.Method != http.MethodGet && req.r.Method != http.MethodHead {
			return s3err.MethodNotAllowed()
		}
		http.Redirect(req.w, req.r, console.Prefix, http.StatusMovedPermanently)
		return nil
	case p == consoleUsagePath:
		return h.consoleUsage(req)
	default:
		consoleFiles.ServeHTTP(req.w, req.r)
		return nil
	}
}

type consoleBucket struct {
	Name        string    `json:"name,omitempty"`
	Objects     int64     `json:"objects"`
	Bytes       int64     `json:"bytes"`
	MeasuredAt  time.Time `json:"measured_at,omitzero"`
	MaxSize     int64     `json:"max_size,omitempty"`
	ExpireAfter string    `json:"expire_after,omitempty"`
	Evict       bool      `json:"evict,omitempty"`
	Public      bool      `json:"public,omitempty"`
	Website     bool      `json:"website,omitempty"`
	CORS        bool      `json:"cors,omitempty"`
}

type consoleUsageResponse struct {
	// ReadOnly tells the console to hide what the signing key may not do.
	ReadOnly bool            `json:"read_only,omitempty"`
	Buckets  []consoleBucket `json:"buckets"`
	// Total is reported only to a key that may see every bucket.
	Total *consoleBucket `json:"total,omitempty"`
}

// consoleUsage reports the usage of the buckets the signing key may access, as writes
// and deletes keep it between the janitor's measurements.
func (h *Handler) consoleUsage(req *request) error {
	if req.r.Method != http.MethodGet {
		return s3err.MethodNotAllowed()
	}
	req.q = req.r.URL.Query()
	if anonymous(req.r, req.q) {
		return s3err.AccessDenied("The console API needs a signed request.")
	}
	auth, e := sigv4.Verify(req.r, h.credentials, h.now())
	if e != nil {
		return e
	}
	req.auth, req.cred = auth, h.access.Load().creds[auth.AccessKey]

	buckets, err := h.store.ListBuckets()
	if err != nil {
		return err
	}
	usage := h.store.AllBucketUsage()
	res := consoleUsageResponse{ReadOnly: req.cred.ReadOnly, Buckets: []consoleBucket{}}
	// The total sums the tracked per-bucket figures rather than the last walk, so it
	// matches the rows; it is dated by that walk.
	total := consoleBucket{MeasuredAt: h.store.Usage().At}
	for _, b := range buckets {
		if !req.cred.allows(b.Name) {
			continue
		}
		u, l, p := usage[b.Name], h.store.BucketLimit(b.Name), h.policy(b.Name)
		item := consoleBucket{Name: b.Name, Objects: u.Objects, Bytes: u.Bytes, MeasuredAt: u.At,
			MaxSize: l.MaxSize, Evict: l.Evict, Public: p.Public, Website: p.Website != nil, CORS: p.CORS != nil}
		if l.ExpireAfter > 0 {
			item.ExpireAfter = l.ExpireAfter.String()
		}
		res.Buckets = append(res.Buckets, item)
		total.Objects += item.Objects
		total.Bytes += item.Bytes
	}
	if len(req.cred.Buckets) == 0 {
		res.Total = &total
	}
	body, err := json.Marshal(res)
	if err != nil {
		return err
	}
	hdr := req.w.Header()
	console.SetSecurityHeaders(hdr)
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	_, _ = req.w.Write(body)
	return nil
}
