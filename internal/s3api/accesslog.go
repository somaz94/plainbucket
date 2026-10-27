package s3api

import (
	"log/slog"
	"net/url"
	"time"

	"github.com/somaz94/plainbucket/internal/httpx"
)

// logAccess logs a finished request at info level; op is its metric operation.
func (h *Handler) logAccess(req *request, rec *httpx.Recorder, op string, start time.Time) {
	r, id := req.r, req.id
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
