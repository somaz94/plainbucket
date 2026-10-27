package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// receiver is a webhook endpoint that records what it is sent.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	got    []*http.Request
	bodies [][]byte
	status func(n int) int // the status of the n-th request, from 1
}

func newReceiver(t *testing.T, status func(n int) int) *receiver {
	rc := &receiver{status: status}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, r)
		rc.bodies = append(rc.bodies, body)
		n := len(rc.got)
		rc.mu.Unlock()
		code := http.StatusNoContent
		if rc.status != nil {
			code = rc.status(n)
		}
		if code == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

func newNotifier(t *testing.T) *Notifier {
	n := New("us-east-1", slog.New(slog.DiscardHandler))
	n.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { n.Close(context.Background()) })
	return n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestValidEvent(t *testing.T) {
	for p, want := range map[string]bool{
		"s3:ObjectCreated:*": true, "s3:ObjectCreated:Put": true, "s3:ObjectCreated:CompleteMultipartUpload": true,
		"s3:ObjectRemoved:*": true, "s3:ObjectRemoved:Delete": true, "s3:LifecycleExpiration:*": true,
		"s3:LifecycleExpiration:Delete": true,
		"ObjectCreated:Put":             false, "s3:ObjectCreated": false, "s3:*": false, "s3:ObjectCreated:P*": false,
		"s3:ObjectRemoved:DeleteMarkerCreated": false, "s3:ObjectTagging:*": false, "": false,
	} {
		if ValidEvent(p) != want {
			t.Errorf("ValidEvent(%q) = %v", p, !want)
		}
	}
}

func TestTargetWants(t *testing.T) {
	tg := Target{Events: []string{"s3:ObjectCreated:*", "s3:LifecycleExpiration:Delete"}, Prefix: "logs/", Suffix: ".gz"}
	for _, c := range []struct {
		name, key string
		want      bool
	}{
		{ObjectCreatedPut, "logs/a.gz", true},
		{ObjectCreatedComplete, "logs/b.gz", true},
		{LifecycleExpiration, "logs/c.gz", true},
		{ObjectRemovedDelete, "logs/a.gz", false},
		{ObjectCreatedPut, "other/a.gz", false},
		{ObjectCreatedPut, "logs/a.txt", false},
	} {
		if got := tg.wants(Event{Name: c.name, Key: c.key}); got != c.want {
			t.Errorf("%s %s: %v", c.name, c.key, got)
		}
	}
	if !(Target{}).wants(Event{Name: ObjectRemovedDelete, Key: "k"}) {
		t.Error("no events listed should mean all")
	}
}

func TestDeliverySignedRecord(t *testing.T) {
	rc := newReceiver(t, nil)
	n := newNotifier(t)
	now := time.Date(2026, 10, 26, 1, 2, 3, 456e6, time.UTC)
	n.now = func() time.Time { return now }
	secret := []byte("s3cret")
	n.SetTargets(map[string][]Target{"bkt": {{ID: "hook", URL: rc.URL + "/in", Secret: secret}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "a b/ü+c", Size: 5, ETag: `"abc"`,
		Principal: "AKID", SourceIP: "192.0.2.7", RequestID: "REQ1", Time: now})
	n.Publish(Event{Name: ObjectRemovedDelete, Bucket: "bkt", Key: "gone", Time: now})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "other", Key: "k", Time: now})
	eventually(t, "two deliveries", func() bool { return rc.count() == 2 })

	r, body := rc.got[0], rc.bodies[0]
	ts := r.Header.Get(HeaderTimestamp)
	if ts != strconv.FormatInt(now.Unix(), 10) || r.Header.Get(HeaderSignature) != Sign(secret, ts, body) {
		t.Errorf("signature headers: %v", r.Header)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/in" || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get(HeaderEvent) != ObjectCreatedPut {
		t.Errorf("request: %s %s %v", r.Method, r.URL, r.Header)
	}
	var msg message
	if err := json.Unmarshal(body, &msg); err != nil || len(msg.Records) != 1 {
		t.Fatalf("body %s: %v", body, err)
	}
	rec := msg.Records[0]
	if rec.EventVersion != "2.1" || rec.EventSource != "aws:s3" || rec.AWSRegion != "us-east-1" ||
		rec.EventTime != "2026-10-26T01:02:03.456Z" || rec.EventName != ObjectCreatedPut ||
		rec.UserIdentity.PrincipalID != "AKID" || rec.RequestParameters.SourceIPAddress != "192.0.2.7" ||
		rec.ResponseElements["x-amz-request-id"] != "REQ1" {
		t.Errorf("record: %+v", rec)
	}
	o := rec.S3.Object
	if rec.S3.ConfigurationID != "hook" || rec.S3.Bucket.Name != "bkt" || rec.S3.Bucket.ARN != "arn:aws:s3:::bkt" ||
		o.Key != "a+b/%C3%BC%2Bc" || o.Size == nil || *o.Size != 5 || o.ETag != "abc" || len(o.Sequencer) != 16 {
		t.Errorf("s3 entity: %+v object %+v", rec.S3, o)
	}

	var del message
	_ = json.Unmarshal(rc.bodies[1], &del)
	d := del.Records[0]
	if d.S3.Object.Size != nil || d.S3.Object.ETag != "" || d.UserIdentity.PrincipalID != ownerID {
		t.Errorf("delete record: %+v", d)
	}
	if d.S3.Object.Sequencer <= o.Sequencer {
		t.Errorf("sequencer %s not after %s", d.S3.Object.Sequencer, o.Sequencer)
	}
	if sent, failed, dropped, queued := n.Stats(); sent != 2 || failed != 0 || dropped != 0 || queued != 0 {
		t.Errorf("stats %d %d %d %d", sent, failed, dropped, queued)
	}
}

func TestDeliveryUnsigned(t *testing.T) {
	rc := newReceiver(t, nil)
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	eventually(t, "a delivery", func() bool { return rc.count() == 1 })
	if h := rc.got[0].Header; h.Get(HeaderSignature) != "" || h.Get(HeaderTimestamp) != "" {
		t.Errorf("unsigned target sent %v", h)
	}
}

func TestDeliveryRetries(t *testing.T) {
	rc := newReceiver(t, func(n int) int {
		switch n {
		case 1:
			return http.StatusServiceUnavailable
		case 2:
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	})
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	eventually(t, "the third attempt", func() bool { s, _, _, _ := n.Stats(); return s == 1 })
	if rc.count() != 3 {
		t.Errorf("attempts %d", rc.count())
	}
}

func TestDeliveryGivesUp(t *testing.T) {
	for name, status := range map[string]int{"refused": http.StatusBadRequest, "redirect": http.StatusFound, "server error": 500} {
		t.Run(name, func(t *testing.T) {
			rc := newReceiver(t, func(int) int { return status })
			n := newNotifier(t)
			n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL + "/?token=x"}}})
			n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
			eventually(t, "the failure", func() bool { _, f, _, _ := n.Stats(); return f == 1 })
			want := 1
			if status >= 500 {
				want = 1 + len(n.backoff)
			}
			if rc.count() != want {
				t.Errorf("attempts %d, want %d", rc.count(), want)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		rc := newReceiver(t, nil)
		rc.Close()
		n := newNotifier(t)
		n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
		n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
		eventually(t, "the failure", func() bool { _, f, _, _ := n.Stats(); return f == 1 })
	})
}

// blocking is an endpoint that holds every request until release is closed.
func blocking(t *testing.T) (rc *receiver, entered *atomic.Int64, release chan struct{}) {
	release = make(chan struct{})
	entered = &atomic.Int64{}
	rc = newReceiver(t, func(int) int {
		entered.Add(1)
		<-release
		return http.StatusOK
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return rc, entered, release
}

func TestQueueFullDrops(t *testing.T) {
	rc, entered, release := blocking(t)
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	e := Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()}
	n.Publish(e)
	eventually(t, "the first request", func() bool { return entered.Load() == 1 })
	for range queueSize + 3 {
		n.Publish(e)
	}
	if _, _, dropped, queued := n.Stats(); dropped != 3 || queued != queueSize {
		t.Errorf("dropped %d queued %d", dropped, queued)
	}
	close(release)
	eventually(t, "the queue to empty", func() bool { s, _, _, _ := n.Stats(); return s == queueSize+1 })
}

func TestSetTargets(t *testing.T) {
	a, b := newReceiver(t, nil), newReceiver(t, nil)
	n := newNotifier(t)
	ta := Target{ID: "a", URL: a.URL}
	n.SetTargets(map[string][]Target{"one": {ta}, "two": {ta}})
	first := n.workers[targetKey(ta)]
	n.SetTargets(map[string][]Target{"one": {ta, {ID: "b", URL: b.URL}}})
	if n.workers[targetKey(ta)] != first || len(n.workers) != 2 {
		t.Error("an unchanged target should keep its worker")
	}
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "one", Key: "k", Time: time.Now()})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "two", Key: "k", Time: time.Now()})
	eventually(t, "both targets", func() bool { return a.count() == 1 && b.count() == 1 })

	n.SetTargets(nil)
	select {
	case <-first.stop:
	default:
		t.Error("a removed target's worker was not stopped")
	}
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "one", Key: "k", Time: time.Now()})
	n.Close(context.Background())
	n.SetTargets(map[string][]Target{"one": {ta}})
	if a.count() != 1 || len(n.workers) != 0 {
		t.Errorf("sent after removal or close: %d", a.count())
	}
}

func TestCloseDrains(t *testing.T) {
	rc := newReceiver(t, nil)
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	for range 5 {
		n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	}
	n.Close(context.Background())
	if rc.count() != 5 {
		t.Errorf("delivered %d of 5 before Close returned", rc.count())
	}
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	if _, _, _, q := n.Stats(); q != 0 {
		t.Error("published after Close")
	}
}

func TestCloseGivesUp(t *testing.T) {
	rc, entered, _ := blocking(t)
	n := newNotifier(t)
	n.backoff = nil // the attempt cut short is the last one, and still counts as dropped
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	for range 3 {
		n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	}
	eventually(t, "the first request", func() bool { return entered.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	n.Close(ctx)
	if sent, failed, dropped, _ := n.Stats(); sent != 0 || failed != 0 || dropped != 3 {
		t.Errorf("sent %d failed %d dropped %d", sent, failed, dropped)
	}
}

func TestPublishDuringSetTargets(t *testing.T) {
	rc := newReceiver(t, nil)
	n := newNotifier(t)
	target := func(secret string) map[string][]Target {
		return map[string][]Target{"bkt": {{URL: rc.URL, Secret: []byte(secret)}}}
	}
	n.SetTargets(target("a"))
	const publishers, each = 4, 50
	var wg sync.WaitGroup
	for range publishers {
		wg.Go(func() {
			for range each {
				n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
			}
		})
	}
	// A rotated secret replaces the worker while events are published.
	for i := range 20 {
		n.SetTargets(target(strconv.Itoa(i)))
	}
	wg.Wait()
	n.Close(context.Background())
	if sent, failed, dropped, _ := n.Stats(); sent+failed+dropped != publishers*each {
		t.Errorf("sent %d failed %d dropped %d of %d", sent, failed, dropped, publishers*each)
	}
}

func TestErrorLogHidesTheQuery(t *testing.T) {
	rc := newReceiver(t, nil)
	rc.Close()
	var logs bytes.Buffer
	n := New("us-east-1", slog.New(slog.NewTextHandler(&logs, nil)))
	n.backoff = nil
	defer n.Close(context.Background())
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL + "/?token=hunter2"}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	eventually(t, "the failure", func() bool { _, f, _, _ := n.Stats(); return f == 1 })
	n.Close(context.Background())
	if strings.Contains(logs.String(), "hunter2") || !strings.Contains(logs.String(), "event notification failed") {
		t.Errorf("log: %s", logs.String())
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@hooks.example.com/p?token=x#f": "https://hooks.example.com/p",
		"http://hooks.example.com":                      "http://hooks.example.com",
		"://bad":                                        "",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q", in, got)
		}
	}
}

func TestSequencerIncreases(t *testing.T) {
	n := New("r", slog.New(slog.DiscardHandler))
	at := time.Unix(100, 0)
	a, b := n.sequencer(at), n.sequencer(at)
	if b <= a || n.sequencer(at.Add(-time.Hour)) <= b {
		t.Errorf("sequencer did not increase: %s %s", a, b)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":            0,
		"soon":        0,
		"3":           3 * time.Second,
		"-5":          0,
		"99999999999": maxRetryAfter,
		now.Add(10 * time.Second).Format(http.TimeFormat): 10 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
		now.Add(time.Hour).Format(http.TimeFormat):        maxRetryAfter,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestDeliveryWaitsForRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		first := len(at) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(srv.Close)
	n := newNotifier(t) // backoff of a millisecond
	n.SetTargets(map[string][]Target{"bkt": {{URL: srv.URL}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "k", Time: time.Now()})
	eventually(t, "the retry", func() bool { s, _, _, _ := n.Stats(); return s == 1 })
	mu.Lock()
	defer mu.Unlock()
	if gap := at[1].Sub(at[0]); gap < 900*time.Millisecond {
		t.Errorf("retried after %v, before the receiver's Retry-After", gap)
	}
}

func TestRotatedSecretKeepsTheWorker(t *testing.T) {
	release := make(chan struct{})
	var first sync.Once
	rc := newReceiver(t, func(int) int {
		first.Do(func() { <-release })
		return http.StatusNoContent
	})
	n := newNotifier(t)
	old := Target{ID: "hook", URL: rc.URL, Secret: []byte("old")}
	n.SetTargets(map[string][]Target{"bkt": {old}})
	w := n.workers[targetKey(old)]
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "a", Time: time.Now()})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "b", Time: time.Now()})
	eventually(t, "the first delivery", func() bool { return rc.count() == 1 })
	rotated := old
	rotated.Secret = []byte("new")
	n.SetTargets(map[string][]Target{"bkt": {rotated}})
	if n.workers[targetKey(rotated)] != w || isClosed(w.stop) {
		t.Fatal("a rotated secret replaced the worker")
	}
	close(release)
	eventually(t, "both deliveries", func() bool { return rc.count() == 2 })
	rc.mu.Lock()
	defer rc.mu.Unlock()
	r, body := rc.got[1], rc.bodies[1]
	if r.Header.Get(HeaderSignature) != Sign([]byte("new"), r.Header.Get(HeaderTimestamp), body) {
		t.Error("the queued event was not signed with the rotated secret")
	}
}

// Buckets sharing a webhook with their own secrets share its worker but not their keys.
func TestSharedWebhookSignsPerBucket(t *testing.T) {
	rc := newReceiver(t, nil)
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{
		"one": {{ID: "hook", URL: rc.URL, Secret: []byte("key-one")}},
		"two": {{ID: "hook", URL: rc.URL, Secret: []byte("key-two")}},
	})
	if len(n.workers) != 1 {
		t.Errorf("%d workers for one webhook", len(n.workers))
	}
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "one", Key: "k", Time: time.Now()})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "two", Key: "k", Time: time.Now()})
	eventually(t, "both deliveries", func() bool { return rc.count() == 2 })
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i, r := range rc.got {
		var msg message
		if err := json.Unmarshal(rc.bodies[i], &msg); err != nil || len(msg.Records) != 1 {
			t.Fatalf("body %s: %v", rc.bodies[i], err)
		}
		key := []byte("key-" + msg.Records[0].S3.Bucket.Name)
		if r.Header.Get(HeaderSignature) != Sign(key, r.Header.Get(HeaderTimestamp), rc.bodies[i]) {
			t.Errorf("%s: not signed with its bucket's secret", msg.Records[0].S3.Bucket.Name)
		}
	}
}

// A target a reload removes gets one attempt per event it still holds, not the retries.
func TestRemovedTargetTriesOnce(t *testing.T) {
	release := make(chan struct{})
	var first sync.Once
	rc := newReceiver(t, func(int) int {
		first.Do(func() { <-release })
		return http.StatusServiceUnavailable
	})
	n := newNotifier(t)
	n.SetTargets(map[string][]Target{"bkt": {{URL: rc.URL}}})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "a", Time: time.Now()})
	n.Publish(Event{Name: ObjectCreatedPut, Bucket: "bkt", Key: "b", Time: time.Now()})
	eventually(t, "the first attempt", func() bool { return rc.count() == 1 })
	n.SetTargets(nil)
	close(release)
	eventually(t, "both failures", func() bool { _, failed, _, _ := n.Stats(); return failed == 2 })
	if rc.count() != 2 {
		t.Errorf("%d attempts for two events of a removed target, want 2", rc.count())
	}
}
