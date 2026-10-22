package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecorder(t *testing.T) {
	w := httptest.NewRecorder()
	r := NewRecorder(w)
	if r.Status != http.StatusOK {
		t.Fatalf("default status %d", r.Status)
	}
	r.WriteHeader(http.StatusTeapot)
	if _, err := r.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	r.Flush()
	if r.Status != http.StatusTeapot || r.Bytes != 3 || w.Code != http.StatusTeapot || !w.Flushed {
		t.Fatalf("status=%d bytes=%d inner=%d flushed=%v", r.Status, r.Bytes, w.Code, w.Flushed)
	}
	if r.Unwrap() != w {
		t.Fatal("Unwrap must return the wrapped writer")
	}
}

// readerFromWriter records that ReadFrom was used, standing in for net/http's sendfile path.
type readerFromWriter struct {
	http.ResponseWriter
	used bool
}

func (w *readerFromWriter) ReadFrom(src io.Reader) (int64, error) {
	w.used = true
	return io.Copy(w.ResponseWriter, src)
}

func TestRecorderReadFrom(t *testing.T) {
	inner := &readerFromWriter{ResponseWriter: httptest.NewRecorder()}
	r := NewRecorder(inner)
	n, err := r.ReadFrom(strings.NewReader("hello"))
	if err != nil || n != 5 || r.Bytes != 5 || !inner.used {
		t.Fatalf("forwarded ReadFrom: n=%d err=%v bytes=%d used=%v", n, err, r.Bytes, inner.used)
	}

	// A writer without ReadFrom still counts bytes through Write.
	plain := NewRecorder(struct{ http.ResponseWriter }{httptest.NewRecorder()})
	if n, err := plain.ReadFrom(strings.NewReader("abcd")); err != nil || n != 4 || plain.Bytes != 4 {
		t.Fatalf("fallback ReadFrom: n=%d err=%v bytes=%d", n, err, plain.Bytes)
	}
}

func TestRecorderFlushWithoutFlusher(t *testing.T) {
	NewRecorder(struct{ http.ResponseWriter }{httptest.NewRecorder()}).Flush()
}
