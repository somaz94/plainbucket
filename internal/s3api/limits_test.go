package s3api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

func TestMetricsEndpoint(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.Metrics = true })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/k", []byte("hello"), nil, http.StatusOK)
	x.mustDo(http.MethodGet, "/bkt/k", nil, nil, http.StatusOK)
	x.do(http.MethodGet, "/bkt/missing", nil, nil)

	scrape := func() string {
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics: %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := scrape()
	for _, want := range []string{
		`plainbucket_requests_total{method="PUT",status="200"} 2`,
		`plainbucket_requests_total{method="GET",status="404"} 1`,
		`plainbucket_request_duration_seconds_bucket{le="+Inf"} 4`,
		`plainbucket_request_duration_seconds_count 4`,
		"plainbucket_received_bytes_total 5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "plainbucket_objects") {
		t.Error("usage gauges before the first scan")
	}
	x.h.store.MeasureUsage(context.Background())
	if body := scrape(); !strings.Contains(body, "plainbucket_objects 1") || !strings.Contains(body, "plainbucket_stored_bytes 5") {
		t.Errorf("usage gauges:\n%s", body)
	}
	if strings.Contains(body, "plainbucket_bucket_bytes") || !strings.Contains(body, "plainbucket_evicted_objects_total 0") {
		t.Errorf("limit metrics without limits:\n%s", body)
	}
	x.h.store.SetLimits(map[string]storage.Limit{"bkt": {MaxSize: 100}})
	x.h.store.MeasureUsage(context.Background())
	if body := scrape(); !strings.Contains(body, `plainbucket_bucket_bytes{bucket="bkt"} 5`) || !strings.Contains(body, `plainbucket_bucket_objects{bucket="bkt"} 1`) {
		t.Errorf("per-bucket gauges:\n%s", body)
	}
}

func TestQuotaExceededAPI(t *testing.T) {
	x := newHarness(t, nil)
	x.h.store.SetLimits(map[string]storage.Limit{"bkt": {MaxSize: 4}})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/a", []byte("1234"), nil, http.StatusOK)
	rec := x.do(http.MethodPut, "/bkt/b", []byte("5"), nil)
	if rec.Code != http.StatusForbidden || errCode(rec) != "QuotaExceeded" {
		t.Errorf("over quota: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMetricsDisabled(t *testing.T) {
	x := newHarness(t, nil)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("metrics served while disabled: %d", rec.Code)
	}
}

// trickle writes the first chunk, then stalls until the test ends.
type trickle struct {
	first []byte
	stop  chan struct{}
}

func (r *trickle) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	<-r.stop
	return 0, io.EOF
}

func TestSlowUploadIsCut(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.BodyTimeout = 200 * time.Millisecond })
	x.h.now = time.Now
	srv := httptest.NewServer(x.h)
	defer srv.Close()

	sign := func(r *http.Request) {
		x.signer.Sign(r, sigv4.UnsignedPayload, time.Now())
	}
	mk, _ := http.NewRequest(http.MethodPut, srv.URL+"/bkt", nil)
	sign(mk)
	if resp, err := srv.Client().Do(mk); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %v %v", resp, err)
	}

	stop := make(chan struct{})
	defer close(stop)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bkt/slow", &trickle{first: []byte("abc"), stop: stop})
	req.ContentLength = 1 << 20
	sign(req)
	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("stalled upload accepted: %d", resp.StatusCode)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stalled upload held for %v", d)
	}

	// An upload that keeps delivering is not cut even though it outlasts the timeout.
	pr, pw := io.Pipe()
	go func() {
		chunk := bytes.Repeat([]byte("x"), minProgress)
		for range 6 {
			_, _ = pw.Write(chunk)
			time.Sleep(100 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/bkt/steady", pr)
	req.ContentLength = 6 * minProgress
	sign(req)
	resp, err = srv.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("steady upload: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}
