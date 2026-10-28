package s3api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/console"
	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	consoleUsagePath = console.Prefix + "api/usage"
	consoleTrashPath = console.Prefix + "api/trash"
)

var consoleFiles = console.Handler()

// isConsolePath reports whether a path-style request belongs to the console.
func isConsolePath(p string) bool {
	return p == strings.TrimSuffix(console.Prefix, "/") || strings.HasPrefix(p, console.Prefix)
}

// serveConsole answers the console's files to anyone, and its usage and trash APIs to
// signed requests. Restoring and purging trash are its only writes outside the S3 API,
// which has no operation for them.
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
	case p == consoleTrashPath:
		return h.consoleTrash(req)
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
	Lifecycle   int       `json:"lifecycle_rules,omitempty"`
	Evict       bool      `json:"evict,omitempty"`
	Public      bool      `json:"public,omitempty"`
	Website     bool      `json:"website,omitempty"`
	CORS        bool      `json:"cors,omitempty"`
	TrashAfter  string    `json:"trash_after,omitempty"`
}

type consoleUsageResponse struct {
	// ReadOnly tells the console to hide writes: the key is read-only or the server is in maintenance.
	ReadOnly bool `json:"read_only,omitempty"`
	// Maintenance is the server-wide read-only mode alone.
	Maintenance bool            `json:"maintenance,omitempty"`
	Buckets     []consoleBucket `json:"buckets"`
	// Total and Disk are reported only to a key that may see every bucket.
	Total *consoleBucket `json:"total,omitempty"`
	Disk  *consoleDisk   `json:"disk,omitempty"`
}

type consoleDisk struct {
	Free    int64 `json:"free"`
	Size    int64 `json:"size"`
	MinFree int64 `json:"min_free,omitempty"`
}

// consoleUsage reports the usage of the buckets the signing key may access, as writes
// and deletes keep it between the janitor's measurements.
func (h *Handler) consoleUsage(req *request) error {
	if req.r.Method != http.MethodGet {
		return s3err.MethodNotAllowed()
	}
	if err := h.consoleAuth(req); err != nil {
		return err
	}

	buckets, err := h.listing(storageTimeout)
	if errors.Is(err, storage.ErrProbeTimeout) {
		req.w.Header().Set("Retry-After", storageRetry)
		return s3err.ServiceUnavailable("The storage did not answer in time.")
	}
	if err != nil {
		return err
	}
	usage, limits := h.store.AllBucketUsage(), h.store.Limits()
	res := consoleUsageResponse{ReadOnly: req.cred.ReadOnly || req.readOnly, Maintenance: req.readOnly, Buckets: []consoleBucket{}}
	// The total sums the tracked per-bucket figures rather than the last walk, so it
	// matches the rows; it is dated by that walk.
	total := consoleBucket{MeasuredAt: h.store.Usage().At}
	for _, b := range buckets {
		if !req.cred.allows(b.Name) {
			continue
		}
		u, l, p := usage[b.Name], limits[b.Name], h.policy(b.Name)
		item := consoleBucket{Name: b.Name, Objects: u.Objects, Bytes: u.Bytes, MeasuredAt: u.At,
			MaxSize: l.MaxSize, Evict: l.Evict, Lifecycle: len(l.Rules), Public: p.Public, Website: p.Website != nil, CORS: p.CORS != nil}
		if l.ExpireAfter > 0 {
			item.ExpireAfter = l.ExpireAfter.String()
		}
		if t := h.store.TrashAfter(b.Name); t > 0 {
			item.TrashAfter = t.String()
		}
		res.Buckets = append(res.Buckets, item)
		total.Objects += item.Objects
		total.Bytes += item.Bytes
	}
	if len(req.cred.Buckets) == 0 {
		res.Total = &total
		if free, size, err := h.store.DiskSpaceWithin(storageTimeout); err == nil {
			res.Disk = &consoleDisk{Free: free, Size: size, MinFree: h.store.MinFree()}
		}
	}
	return writeConsoleJSON(req, res)
}

// consoleAuth verifies a console API request's signature and sets req.q, req.auth and req.cred.
func (h *Handler) consoleAuth(req *request) error {
	req.q = req.r.URL.Query()
	if anonymous(req.r, req.q) {
		return s3err.AccessDenied("The console API needs a signed request.")
	}
	if err := h.verifySigned(req); err != nil {
		return err
	}
	return h.throttle(req, h.keyLimiter(req), false)
}

func writeConsoleJSON(req *request, v any) error {
	body, err := json.Marshal(v)
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

type consoleTrashResponse struct {
	TrashAfter string `json:"trash_after,omitempty"`
	// Bytes is the disk space the entries take; they count against no limit.
	Bytes   int64                `json:"bytes"`
	Entries []storage.TrashEntry `json:"entries"`
}

// consoleTrash lists a bucket's trash (GET ?bucket=) or restores or purges one entry
// (POST ?bucket=&id=&action=restore|purge), within the signing key's limits.
func (h *Handler) consoleTrash(req *request) error {
	m := req.r.Method
	if m != http.MethodGet && m != http.MethodPost {
		return s3err.MethodNotAllowed()
	}
	if err := h.consoleAuth(req); err != nil {
		return err
	}
	req.bucket = req.q.Get("bucket")
	if err := req.authorize(); err != nil {
		return err
	}
	if err := h.store.HeadBucket(req.bucket); err != nil {
		return err
	}
	if m == http.MethodGet {
		entries, err := h.store.ListTrash(req.bucket)
		if err != nil {
			return err
		}
		res := consoleTrashResponse{Entries: entries}
		for _, e := range entries {
			res.Bytes += e.Size
		}
		if t := h.store.TrashAfter(req.bucket); t > 0 {
			res.TrashAfter = t.String()
		}
		return writeConsoleJSON(req, res)
	}
	id := req.q.Get("id")
	switch req.q.Get("action") {
	case "restore":
		info, err := h.store.RestoreTrash(req.bucket, id)
		if err != nil {
			return err
		}
		h.publish(req, notify.ObjectCreatedPut, info.Key, info)
		return writeConsoleJSON(req, map[string]string{"key": info.Key, "etag": info.ETag})
	case "purge":
		if err := h.store.PurgeTrash(req.bucket, id); err != nil {
			return err
		}
		req.w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return s3err.InvalidArgument("action must be restore or purge.")
}
