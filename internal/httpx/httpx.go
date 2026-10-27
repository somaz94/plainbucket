// Package httpx holds the response recorder, metric label helper and header lists of the
// S3 API; it has no dependencies, so the configuration can share the lists.
package httpx

import (
	"io"
	"net/http"
)

// MinBodyProgress is how many body bytes an upload must deliver within each body timeout.
const MinBodyProgress = 32 << 10

// StoredHeaders are kept with an object and returned on GET and HEAD.
var StoredHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// RuleHeaders are the headers a bucket's header rule may set: those an object can store,
// and security headers. Content-Type, CSP and CORS stay the server's, which protect the console.
var RuleHeaders = append(append([]string{}, StoredHeaders...),
	"Permissions-Policy", "Referrer-Policy", "Strict-Transport-Security", "X-Content-Type-Options", "X-Frame-Options")

// MetricMethod maps a request method to a bounded metric label. net/http accepts any
// method token, so labelling by r.Method lets a client mint unlimited series.
func MetricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodOptions, http.MethodPatch:
		return method
	}
	return "OTHER"
}

// Recorder wraps an http.ResponseWriter to capture the status code and bytes written.
type Recorder struct {
	http.ResponseWriter
	Status int
	Bytes  int64
	wrote  bool
}

// NewRecorder returns a Recorder whose status defaults to 200, as net/http's does.
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, Status: http.StatusOK}
}

func (r *Recorder) WriteHeader(code int) {
	r.Status, r.wrote = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += int64(n)
	return n, err
}

// ReadFrom forwards to the wrapped writer so http.ServeContent keeps its sendfile path.
func (r *Recorder) ReadFrom(src io.Reader) (int64, error) {
	r.wrote = true
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.Bytes += n
		return n, err
	}
	return io.Copy(writerOnly{r}, src)
}

// Wrote reports whether any part of the response has been sent.
func (r *Recorder) Wrote() bool { return r.wrote }

// Flush implements http.Flusher when the wrapped writer does.
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		r.wrote = true // net/http sends the header on a flush
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying connection.
func (r *Recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// writerOnly hides Recorder's ReadFrom so io.Copy does not recurse into it.
type writerOnly struct{ io.Writer }
