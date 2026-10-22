package s3api

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

// BucketPolicy is the configured behavior of one bucket.
type BucketPolicy struct {
	// Public lets anyone read the bucket's objects with an unsigned GET or HEAD.
	Public bool
	// Website serves index and error documents to unsigned requests; needs Public.
	Website *Website
	CORS    *CORS
}

// Website names the documents of a bucket served as a static site.
type Website struct {
	Index string // served for the bucket root and every key ending in /
	Error string // served with 404 for a missing key; empty answers with S3's XML error
}

// CORS lists what browsers on other origins may do with a bucket.
type CORS struct {
	AllowedOrigins []string // exact origins, "*", or one wildcard such as https://*.example.com
	AllowedMethods []string // empty means GET and HEAD
	AllowedHeaders []string // request headers a preflight may ask for; "*" allows any
	ExposeHeaders  []string
	MaxAge         time.Duration
}

// wildcardMatch reports whether s matches pattern, which may hold one "*".
func wildcardMatch(pattern, s string) bool {
	pre, suf, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == s
	}
	return len(s) >= len(pre)+len(suf) && strings.HasPrefix(s, pre) && strings.HasSuffix(s, suf)
}

func (c *CORS) allowsOrigin(origin string) bool {
	return slices.ContainsFunc(c.AllowedOrigins, func(o string) bool { return wildcardMatch(o, origin) })
}

func (c *CORS) methods() []string {
	if len(c.AllowedMethods) == 0 {
		return []string{http.MethodGet, http.MethodHead}
	}
	return c.AllowedMethods
}

// allowsHeaders reports whether every header a preflight asks for matches an allowed
// one; header names compare case-insensitively.
func (c *CORS) allowsHeaders(requested string) bool {
	for h := range strings.SplitSeq(requested, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !slices.ContainsFunc(c.AllowedHeaders, func(a string) bool { return wildcardMatch(strings.ToLower(a), h) }) {
			return false
		}
	}
	return true
}

func (h *Handler) policy(bucket string) *BucketPolicy {
	if p, ok := h.buckets[bucket]; ok {
		return p
	}
	return &BucketPolicy{}
}

// applyCORS adds the CORS response headers for an allowed cross-origin request. It
// answers a preflight itself and reports whether it did.
func (h *Handler) applyCORS(req *request) (bool, error) {
	c := h.policy(req.bucket).CORS
	if c == nil {
		return false, nil
	}
	hdr := req.w.Header()
	// Also without an Origin: a cache must not serve this response to another origin.
	hdr.Add("Vary", "Origin")
	origin := req.r.Header.Get("Origin")
	if origin == "" {
		return false, nil
	}
	preflight := req.r.Method == http.MethodOptions && req.r.Header.Get("Access-Control-Request-Method") != ""
	if !c.allowsOrigin(origin) {
		if preflight {
			return true, s3err.AccessDenied("CORSResponse: This CORS request is not allowed.")
		}
		return false, nil
	}
	if !preflight {
		hdr.Set("Access-Control-Allow-Origin", origin)
		if len(c.ExposeHeaders) > 0 {
			hdr.Set("Access-Control-Expose-Headers", strings.Join(c.ExposeHeaders, ", "))
		}
		return false, nil
	}
	method := req.r.Header.Get("Access-Control-Request-Method")
	requested := req.r.Header.Get("Access-Control-Request-Headers")
	if !slices.Contains(c.methods(), method) || !c.allowsHeaders(requested) {
		return true, s3err.AccessDenied("CORSResponse: This CORS request is not allowed.")
	}
	hdr.Set("Access-Control-Allow-Origin", origin)
	hdr.Set("Access-Control-Allow-Methods", strings.Join(c.methods(), ", "))
	if requested != "" {
		hdr.Set("Access-Control-Allow-Headers", requested)
	}
	if c.MaxAge > 0 {
		hdr.Set("Access-Control-Max-Age", strconv.Itoa(int(c.MaxAge.Seconds())))
	}
	req.w.WriteHeader(http.StatusOK)
	return true, nil
}

// websiteKey turns an unsigned read of a website bucket's root or of a key ending in /
// into a read of that prefix's index document.
func (h *Handler) websiteKey(req *request) {
	w := h.policy(req.bucket).Website
	m := req.r.Method
	if w == nil || (m != http.MethodGet && m != http.MethodHead) {
		return
	}
	req.website = true
	if req.key == "" || strings.HasSuffix(req.key, "/") {
		req.key += w.Index
	}
}

// websiteNotFound answers a website request for a missing key: a redirect to key/ when
// that prefix has an index document, else the error document with 404.
func (h *Handler) websiteNotFound(req *request, err error) error {
	if !errors.Is(err, storage.ErrNoSuchKey) {
		return err
	}
	w := h.policy(req.bucket).Website
	if !strings.HasSuffix(req.key, "/"+w.Index) && req.key != w.Index {
		if _, ierr := h.store.HeadObject(req.bucket, req.key+"/"+w.Index); ierr == nil {
			http.Redirect(req.w, req.r, req.r.URL.EscapedPath()+"/", http.StatusFound)
			return nil
		}
	}
	if w.Error == "" {
		return err
	}
	f, info, derr := h.store.GetObject(req.bucket, w.Error)
	if derr != nil || f == nil {
		return err
	}
	defer func() { _ = f.Close() }()
	hdr := req.w.Header()
	ct := info.ContentType
	if ct == "" {
		ct = defaultContentType
	}
	hdr.Set("Content-Type", ct)
	hdr.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	req.w.WriteHeader(http.StatusNotFound)
	if req.r.Method != http.MethodHead {
		_, _ = io.Copy(req.w, f)
	}
	return nil
}
