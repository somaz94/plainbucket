package s3api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeReplicator struct {
	mu      sync.Mutex
	changed []string
}

func (f *fakeReplicator) Changed(bucket, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, bucket+"/"+key)
}

func (f *fakeReplicator) Stats() (copied, deleted, failed, dropped int64, queued int) {
	return 3, 1, 2, 0, 5
}

func (f *fakeReplicator) LastCompared() map[string]time.Time {
	return map[string]time.Time{"bkt/r1": time.Unix(1700000000, 0)}
}

func TestReplicatorHears(t *testing.T) {
	rp := &fakeReplicator{}
	x := newHarness(t, func(o *Options) { o.Replicator = rp; o.Metrics = true })
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/a", []byte("x"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/b", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/a"}, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/a?tagging", tagDoc("k", "v"), nil, http.StatusOK)
	x.mustDo(http.MethodDelete, "/bkt/a?tagging", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodDelete, "/bkt/a", nil, nil, http.StatusNoContent)
	x.expectErr(http.MethodPut, "/bkt/c?tagging", tagDoc("k", "v"), nil, "NoSuchKey") // refused: nothing to replicate
	if got := strings.Join(rp.changed, " "); got != "bkt/a bkt/b bkt/a bkt/a bkt/a" {
		t.Errorf("changes heard: %s", got)
	}

	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{`plainbucket_replication_total{result="copied"} 3`, `plainbucket_replication_total{result="failed"} 2`,
		"plainbucket_replication_queue 5", `plainbucket_replication_compared_timestamp_seconds{rule="bkt/r1"} 1700000000`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
