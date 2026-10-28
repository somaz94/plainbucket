package s3api

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/storage"
)

// durationBuckets are the request duration histogram bounds in seconds; uploads and
// downloads of large objects reach the top ones.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// metrics is a Prometheus text-format registry without external dependencies.
type metrics struct {
	mu        sync.Mutex
	requests  map[requestKey]int64
	durations map[string]*histogram // by operation

	receivedBytes atomic.Int64
	sentBytes     atomic.Int64
	throttledKey  atomic.Int64
	throttledAnon atomic.Int64
}

func (m *metrics) throttled(anonymous bool) {
	if anonymous {
		m.throttledAnon.Add(1)
	} else {
		m.throttledKey.Add(1)
	}
}

type requestKey struct {
	operation, method string
	status            int
}

type histogram struct {
	buckets []int64 // per durationBuckets bound, not cumulative
	inf     int64
	sumSec  float64
}

func newMetrics() *metrics {
	return &metrics{requests: map[requestKey]int64{}, durations: map[string]*histogram{}}
}

func (m *metrics) record(op, method string, status int, sent int64, d time.Duration) {
	sec := d.Seconds()
	m.mu.Lock()
	m.requests[requestKey{op, method, status}]++
	h := m.durations[op]
	if h == nil {
		h = &histogram{buckets: make([]int64, len(durationBuckets))}
		m.durations[op] = h
	}
	if i, _ := slices.BinarySearch(durationBuckets, sec); i < len(durationBuckets) {
		h.buckets[i]++
	} else {
		h.inf++
	}
	h.sumSec += sec
	m.mu.Unlock()
	m.sentBytes.Add(sent)
}

// storeStats is what the metrics read from the store.
type storeStats interface {
	Usage() storage.Usage
	AllBucketUsage() map[string]storage.Usage
	Limits() map[string]storage.Limit
	Removed() (expired, evicted int64)
	SyncErrors() int64
	DiskSpaceWithin(timeout time.Duration) (free, size int64, err error)
	LastScrub() (storage.ScrubResult, bool)
	MinFree() int64
}

// scrape is what one /metrics response reads besides the counters in metrics.
type scrape struct {
	store      storeStats
	notifier   *notify.Notifier // a pointer, not an interface: a nil one must compare nil
	replicator Replicator
	readOnly   bool
	allBuckets bool
	writes     int64
}

func (h *Handler) serveMetrics(w http.ResponseWriter) {
	h.metrics.write(w, scrape{
		store:      h.store,
		notifier:   h.opts.Notifier,
		replicator: h.opts.Replicator,
		readOnly:   h.readOnly.Load(),
		allBuckets: h.opts.BucketMetrics,
		writes:     h.writes.Load(),
	})
}

func (m *metrics) write(w http.ResponseWriter, s scrape) {
	st := s.store
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	m.mu.Lock()
	keys := slices.SortedFunc(maps.Keys(m.requests), func(a, b requestKey) int {
		return cmp.Or(strings.Compare(a.operation, b.operation), strings.Compare(a.method, b.method), a.status-b.status)
	})
	counts := make([]int64, len(keys))
	for i, k := range keys {
		counts[i] = m.requests[k]
	}
	ops := slices.Sorted(maps.Keys(m.durations))
	hists := make([]histogram, len(ops))
	for i, op := range ops {
		h := m.durations[op]
		hists[i] = histogram{buckets: slices.Clone(h.buckets), inf: h.inf, sumSec: h.sumSec}
	}
	m.mu.Unlock()

	fmt.Fprintln(w, "# HELP plainbucket_requests_total HTTP requests (S3 API and console) by S3 operation, method and status.")
	fmt.Fprintln(w, "# TYPE plainbucket_requests_total counter")
	for i, k := range keys {
		fmt.Fprintf(w, "plainbucket_requests_total{operation=%q,method=%q,status=\"%d\"} %d\n", k.operation, k.method, k.status, counts[i])
	}

	fmt.Fprintln(w, "# HELP plainbucket_request_duration_seconds HTTP request duration (S3 API and console) by S3 operation.")
	fmt.Fprintln(w, "# TYPE plainbucket_request_duration_seconds histogram")
	for i, op := range ops {
		h := hists[i]
		var cum int64
		for j, le := range durationBuckets {
			cum += h.buckets[j]
			fmt.Fprintf(w, "plainbucket_request_duration_seconds_bucket{operation=%q,le=%q} %d\n", op, strconv.FormatFloat(le, 'g', -1, 64), cum)
		}
		cum += h.inf
		fmt.Fprintf(w, "plainbucket_request_duration_seconds_bucket{operation=%q,le=\"+Inf\"} %d\n", op, cum)
		fmt.Fprintf(w, "plainbucket_request_duration_seconds_sum{operation=%q} %g\n", op, h.sumSec)
		fmt.Fprintf(w, "plainbucket_request_duration_seconds_count{operation=%q} %d\n", op, cum)
	}

	metric := func(kind, name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, kind, name, v)
	}
	metric("counter", "plainbucket_received_bytes_total", "Bytes stored by successful object and part uploads.", m.receivedBytes.Load())
	metric("counter", "plainbucket_sent_bytes_total", "Bytes sent in responses.", m.sentBytes.Load())

	fmt.Fprintln(w, "# HELP plainbucket_throttled_requests_total Requests refused with 503 SlowDown past a rate limit, by client: an access key or an anonymous address.")
	fmt.Fprintln(w, "# TYPE plainbucket_throttled_requests_total counter")
	fmt.Fprintf(w, "plainbucket_throttled_requests_total{client=\"key\"} %d\n", m.throttledKey.Load())
	fmt.Fprintf(w, "plainbucket_throttled_requests_total{client=\"anonymous\"} %d\n", m.throttledAnon.Load())

	expired, evicted := st.Removed()
	metric("counter", "plainbucket_expired_objects_total", "Objects removed by a bucket's expire-after.", expired)
	metric("counter", "plainbucket_evicted_objects_total", "Objects evicted by a bucket's max-size.", evicted)
	metric("counter", "plainbucket_sync_errors_total", "Directory flushes after a rename or a new directory that failed; those changes stand but may not survive a power loss.", st.SyncErrors())

	if s.notifier != nil {
		sent, failed, dropped, queued := s.notifier.Stats()
		fmt.Fprintln(w, "# HELP plainbucket_notifications_total Bucket event notifications by result: sent, failed after retries or refused, dropped on a full queue or at shutdown.")
		fmt.Fprintln(w, "# TYPE plainbucket_notifications_total counter")
		fmt.Fprintf(w, "plainbucket_notifications_total{result=\"sent\"} %d\n", sent)
		fmt.Fprintf(w, "plainbucket_notifications_total{result=\"failed\"} %d\n", failed)
		fmt.Fprintf(w, "plainbucket_notifications_total{result=\"dropped\"} %d\n", dropped)
		metric("gauge", "plainbucket_notification_queue", "Bucket event notifications waiting to be sent.", int64(queued))
	}
	if s.replicator != nil && s.replicator.Rules() > 0 {
		copied, deleted, failed, dropped, queued := s.replicator.Stats()
		fmt.Fprintln(w, "# HELP plainbucket_replication_total Replication by result: keys copied, target keys deleted, keys failed after retries, changes dropped on a full queue.")
		fmt.Fprintln(w, "# TYPE plainbucket_replication_total counter")
		for _, c := range []struct {
			result string
			n      int64
		}{{"copied", copied}, {"deleted", deleted}, {"failed", failed}, {"dropped", dropped}} {
			fmt.Fprintf(w, "plainbucket_replication_total{result=%q} %d\n", c.result, c.n)
		}
		metric("gauge", "plainbucket_replication_queue", "Replication changes waiting to be applied.", int64(queued))
		if last := s.replicator.LastCompared(); len(last) > 0 {
			fmt.Fprintln(w, "# HELP plainbucket_replication_compared_timestamp_seconds When a replication rule last finished comparing the two listings.")
			fmt.Fprintln(w, "# TYPE plainbucket_replication_compared_timestamp_seconds gauge")
			for _, rule := range slices.Sorted(maps.Keys(last)) {
				fmt.Fprintf(w, "plainbucket_replication_compared_timestamp_seconds{rule=%q} %d\n", rule, last[rule].Unix())
			}
		}
	}

	var readOnly int64
	if s.readOnly {
		readOnly = 1
	}
	metric("gauge", "plainbucket_read_only", "1 while the server-wide read-only mode refuses writes.", readOnly)
	metric("gauge", "plainbucket_writes_in_flight", "Requests other than GET and HEAD being served, including those started before read-only mode.", s.writes)

	metric("gauge", "plainbucket_min_free_bytes", "Free bytes writes must leave (min-free-space); 0 when off.", st.MinFree())
	if free, size, err := st.DiskSpaceWithin(storageTimeout); err == nil {
		metric("gauge", "plainbucket_filesystem_free_bytes", "Bytes left for the server on the filesystem holding the root.", free)
		metric("gauge", "plainbucket_filesystem_size_bytes", "Size of the filesystem holding the root.", size)
	}

	// One read of each, so a reload or walk mid-scrape cannot change a value between its uses.
	u, limits, all := st.Usage(), st.Limits(), st.AllBucketUsage()
	// Buckets with limits are listed even before they exist; every bucket only once a
	// walk has measured it, since before that only the changes since startup are known.
	per := map[string]storage.Usage{}
	for name := range limits {
		per[name] = all[name]
	}
	if s.allBuckets && !u.At.IsZero() {
		maps.Copy(per, all)
	}
	if len(per) > 0 {
		names := slices.Sorted(maps.Keys(per))
		fmt.Fprintln(w, "# HELP plainbucket_bucket_objects Files in a bucket, tracked between usage scans.")
		fmt.Fprintln(w, "# TYPE plainbucket_bucket_objects gauge")
		for _, n := range names {
			fmt.Fprintf(w, "plainbucket_bucket_objects{bucket=%q} %d\n", n, per[n].Objects)
		}
		fmt.Fprintln(w, "# HELP plainbucket_bucket_bytes Bytes in a bucket, tracked between usage scans.")
		fmt.Fprintln(w, "# TYPE plainbucket_bucket_bytes gauge")
		for _, n := range names {
			fmt.Fprintf(w, "plainbucket_bucket_bytes{bucket=%q} %d\n", n, per[n].Bytes)
		}
		fmt.Fprintln(w, "# HELP plainbucket_bucket_max_size_bytes A bucket's max-size; evict tells whether reaching it evicts or refuses.")
		fmt.Fprintln(w, "# TYPE plainbucket_bucket_max_size_bytes gauge")
		for _, n := range names {
			if l := limits[n]; l.MaxSize > 0 {
				fmt.Fprintf(w, "plainbucket_bucket_max_size_bytes{bucket=%q,evict=\"%t\"} %d\n", n, l.Evict, l.MaxSize)
			}
		}
	}

	if sc, ok := st.LastScrub(); ok {
		metric("gauge", "plainbucket_scrub_objects", "Objects the last completed scrub verified.", sc.Objects)
		metric("gauge", "plainbucket_scrub_bytes", "Bytes the last completed scrub read.", sc.Bytes)
		metric("gauge", "plainbucket_scrub_skipped_objects", "Objects the last scrub skipped: no trusted metadata, or changed while read.", sc.Skipped)
		metric("gauge", "plainbucket_scrub_unchecked_objects", "Objects the last scrub could not check: nothing recorded to compare their data with.", sc.Unchecked)
		metric("gauge", "plainbucket_scrub_corrupt_objects", "Objects whose data did not match their recorded digests, or could not be read, in the last scrub.", sc.Corrupt)
		metric("gauge", "plainbucket_scrub_timestamp_seconds", "Unix time the last scrub finished.", sc.Finished.Unix())
	}

	if u.At.IsZero() {
		return
	}
	metric("gauge", "plainbucket_objects", "Files in all buckets as of the last usage scan.", u.Objects)
	metric("gauge", "plainbucket_stored_bytes", "Bytes in all buckets as of the last usage scan.", u.Bytes)
	metric("gauge", "plainbucket_usage_scan_timestamp_seconds", "Unix time of the last usage scan.", u.At.Unix())
}
