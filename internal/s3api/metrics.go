package s3api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/storage"
)

// durationBuckets are the request duration histogram bounds in seconds; uploads and
// downloads of large objects reach the top ones.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// metrics is a Prometheus text-format registry without external dependencies.
type metrics struct {
	mu       sync.Mutex
	requests map[string]int64 // "method:status" -> count
	buckets  []int64          // per durationBuckets bound, not cumulative
	inf      int64
	sumSec   float64

	receivedBytes atomic.Int64
	sentBytes     atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]int64{}, buckets: make([]int64, len(durationBuckets))}
}

func (m *metrics) record(method string, status int, sent int64, d time.Duration) {
	sec := d.Seconds()
	m.mu.Lock()
	m.requests[method+":"+strconv.Itoa(status)]++
	if i, _ := slices.BinarySearch(durationBuckets, sec); i < len(durationBuckets) {
		m.buckets[i]++
	} else {
		m.inf++
	}
	m.sumSec += sec
	m.mu.Unlock()
	m.sentBytes.Add(sent)
}

// storeStats is what the metrics read from the store.
type storeStats interface {
	Usage() storage.Usage
	BucketUsage() map[string]storage.Usage
	Removed() (expired, evicted int64)
}

func (m *metrics) serve(w http.ResponseWriter, st storeStats) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	m.mu.Lock()
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	counts := make([]int64, len(keys))
	for i, k := range keys {
		counts[i] = m.requests[k]
	}
	buckets, inf, sum := slices.Clone(m.buckets), m.inf, m.sumSec
	m.mu.Unlock()

	fmt.Fprintln(w, "# HELP plainbucket_requests_total HTTP requests (S3 API and console) by method and status.")
	fmt.Fprintln(w, "# TYPE plainbucket_requests_total counter")
	for i, k := range keys {
		method, status, _ := strings.Cut(k, ":")
		fmt.Fprintf(w, "plainbucket_requests_total{method=%q,status=%q} %d\n", method, status, counts[i])
	}

	fmt.Fprintln(w, "# HELP plainbucket_request_duration_seconds HTTP request duration (S3 API and console).")
	fmt.Fprintln(w, "# TYPE plainbucket_request_duration_seconds histogram")
	var cum int64
	for i, le := range durationBuckets {
		cum += buckets[i]
		fmt.Fprintf(w, "plainbucket_request_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(le, 'g', -1, 64), cum)
	}
	cum += inf
	fmt.Fprintf(w, "plainbucket_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "plainbucket_request_duration_seconds_sum %g\n", sum)
	fmt.Fprintf(w, "plainbucket_request_duration_seconds_count %d\n", cum)

	metric := func(kind, name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, kind, name, v)
	}
	metric("counter", "plainbucket_received_bytes_total", "Bytes stored by successful object and part uploads.", m.receivedBytes.Load())
	metric("counter", "plainbucket_sent_bytes_total", "Bytes sent in responses.", m.sentBytes.Load())

	expired, evicted := st.Removed()
	metric("counter", "plainbucket_expired_objects_total", "Objects removed by a bucket's expire-after.", expired)
	metric("counter", "plainbucket_evicted_objects_total", "Objects evicted by a bucket's max-size.", evicted)

	if per := st.BucketUsage(); len(per) > 0 {
		names := slices.Sorted(maps.Keys(per))
		fmt.Fprintln(w, "# HELP plainbucket_bucket_objects Files in a bucket with limits, tracked between usage scans.")
		fmt.Fprintln(w, "# TYPE plainbucket_bucket_objects gauge")
		for _, n := range names {
			fmt.Fprintf(w, "plainbucket_bucket_objects{bucket=%q} %d\n", n, per[n].Objects)
		}
		fmt.Fprintln(w, "# HELP plainbucket_bucket_bytes Bytes in a bucket with limits, tracked between usage scans.")
		fmt.Fprintln(w, "# TYPE plainbucket_bucket_bytes gauge")
		for _, n := range names {
			fmt.Fprintf(w, "plainbucket_bucket_bytes{bucket=%q} %d\n", n, per[n].Bytes)
		}
	}

	u := st.Usage()
	if u.At.IsZero() {
		return
	}
	metric("gauge", "plainbucket_objects", "Files in all buckets as of the last usage scan.", u.Objects)
	metric("gauge", "plainbucket_stored_bytes", "Bytes in all buckets as of the last usage scan.", u.Bytes)
	metric("gauge", "plainbucket_usage_scan_timestamp_seconds", "Unix time of the last usage scan.", u.At.Unix())
}
