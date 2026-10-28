package s3api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/storage"
)

// shiftingStats moves Usage and Limits to a later state on each call, as a usage walk
// or a reload finishing during a scrape would.
type shiftingStats struct {
	usageCalls, limitCalls, allCalls int
}

func (f *shiftingStats) Usage() storage.Usage {
	f.usageCalls++
	return storage.Usage{Objects: int64(f.usageCalls), At: time.Unix(int64(f.usageCalls), 0)}
}

func (f *shiftingStats) AllBucketUsage() map[string]storage.Usage {
	f.allCalls++
	return map[string]storage.Usage{"bkt": {Objects: 7}}
}

func (f *shiftingStats) Limits() map[string]storage.Limit {
	f.limitCalls++
	return map[string]storage.Limit{"limited": {MaxSize: int64(100 * f.limitCalls)}}
}

func (f *shiftingStats) Removed() (int64, int64)                             { return 0, 0 }
func (f *shiftingStats) SyncErrors() int64                                   { return 0 }
func (f *shiftingStats) DiskSpaceWithin(time.Duration) (int64, int64, error) { return 1, 2, nil }
func (f *shiftingStats) LastScrub() (storage.ScrubResult, bool)              { return storage.ScrubResult{}, false }
func (f *shiftingStats) MinFree() int64                                      { return 0 }

func TestMetricsReadEachStateOnce(t *testing.T) {
	f := &shiftingStats{}
	rec := httptest.NewRecorder()
	newMetrics().write(rec, scrape{store: f, allBuckets: true})
	if f.usageCalls != 1 || f.limitCalls != 1 || f.allCalls != 1 {
		t.Errorf("calls: usage %d, limits %d, all buckets %d; want one each", f.usageCalls, f.limitCalls, f.allCalls)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`plainbucket_bucket_objects{bucket="bkt"} 7`,
		`plainbucket_bucket_objects{bucket="limited"} 0`,
		`plainbucket_bucket_max_size_bytes{bucket="limited",evict="false"} 100`,
		"plainbucket_objects 1\n",
		"plainbucket_usage_scan_timestamp_seconds 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
}
