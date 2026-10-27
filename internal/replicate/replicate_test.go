package replicate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/s3client"
	"github.com/somaz94/plainbucket/internal/storage"
)

type env struct {
	t        *testing.T
	src, dst *storage.Store
	h        http.Handler
	r        *Replicator
	url      string
	logs     *lockedWriter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	open := func() *storage.Store {
		s, err := storage.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.MinPartSize = 5
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	e := &env{t: t, src: open(), dst: open(), logs: &lockedWriter{}}
	e.h = s3api.New(s3api.Options{Store: e.dst, Region: "us-east-1", Credentials: []s3api.Credential{{AccessKey: "AK", SecretKey: "SK"}}})
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	e.url = srv.URL
	for _, b := range []string{"src", "other"} {
		_ = e.src.CreateBucket(b)
	}
	_ = e.dst.CreateBucket("dst")
	e.r = New(e.src, 0, slog.New(slog.NewTextHandler(e.logs, nil)))
	e.r.backoff = []time.Duration{time.Millisecond}
	t.Cleanup(e.r.Close)
	return e
}

type lockedWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (e *env) rule(mut ...func(*Rule)) Rule {
	r := Rule{ID: "r1", Bucket: "src", Endpoint: e.url, Region: "us-east-1", TargetBucket: "dst", AccessKey: "AK", SecretKey: "SK"}
	for _, m := range mut {
		m(&r)
	}
	return r
}

func (e *env) put(key, body string, o storage.PutOptions) storage.ObjectInfo {
	e.t.Helper()
	info, err := e.src.PutObject("src", key, strings.NewReader(body), int64(len(body)), o)
	if err != nil {
		e.t.Fatal(err)
	}
	return info
}

func (e *env) multipart(key string, parts ...string) storage.ObjectInfo {
	e.t.Helper()
	ctx := context.Background()
	id, err := e.src.CreateUpload("src", key, storage.Metadata{ContentType: "application/x-mp"}, storage.MultipartChecksum{})
	if err != nil {
		e.t.Fatal(err)
	}
	var done []storage.CompletePart
	for i, p := range parts {
		info, err := e.src.PutPart("src", key, id, i+1, strings.NewReader(p), int64(len(p)), nil)
		if err != nil {
			e.t.Fatal(err)
		}
		done = append(done, storage.CompletePart{Number: i + 1, ETag: info.ETag})
	}
	info, err := e.src.CompleteUpload(ctx, "src", key, id, done, storage.Conditions{}, 0, storage.CompleteChecksums{})
	if err != nil {
		e.t.Fatal(err)
	}
	return info
}

// target returns the target object's data and info, or ok false when it is missing.
func (e *env) target(key string) (string, storage.ObjectInfo, bool) {
	f, info, err := e.dst.ReadObject("dst", key)
	if errors.Is(err, storage.ErrNoSuchKey) {
		return "", info, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if f == nil {
		return "", info, true
	}
	defer func() { _ = f.Close() }()
	raw, _ := io.ReadAll(f)
	return string(raw), info, true
}

func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	e.t.Fatalf("timed out waiting for %s\n%s", what, e.logs.String())
}

func (e *env) worker(rule Rule) *worker {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	return e.r.workers[rule.identity()]
}

// compareNow runs a comparison on the worker's goroutine and waits for it to finish.
func (e *env) compareNow(rule Rule) {
	e.t.Helper()
	w := e.worker(rule)
	before := w.last.Load()
	e.r.now = func() time.Time { return time.Unix(before+1, 0) }
	w.kick <- struct{}{}
	e.waitFor("a comparison", func() bool { return w.last.Load() > before })
}

func TestStartCopiesEverything(t *testing.T) {
	e := newEnv(t)
	single := e.put("a.txt", "hello", storage.PutOptions{Metadata: storage.Metadata{ContentType: "text/plain",
		UserMeta: map[string]string{"owner": "ci"}, Headers: map[string]string{"Cache-Control": "max-age=60"}, Tags: []storage.Tag{{Key: "t", Value: "1"}}}})
	mp := e.multipart("big.bin", "first", "second-part")
	e.put("dir/", "", storage.PutOptions{})
	// A file written outside the server has no sidecar: reading it computes its MD5.
	if err := os.WriteFile(filepath.Join(e.src.Root(), "src", "dropped"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	e.waitFor("the first comparison", func() bool { return e.worker(rule).last.Load() > 0 })

	if body, info, ok := e.target("a.txt"); !ok || body != "hello" || info.ETag != single.ETag || info.ContentType != "text/plain" ||
		info.UserMeta["owner"] != "ci" || info.UserMeta[ETagMeta] != single.ETag || info.Headers["Cache-Control"] != "max-age=60" ||
		len(info.Tags) != 1 || info.Tags[0].Value != "1" {
		t.Errorf("single: %q %+v %v", body, info, ok)
	}
	if body, info, ok := e.target("big.bin"); !ok || body != "firstsecond-part" || info.ETag != mp.ETag || info.ContentType != "application/x-mp" {
		t.Errorf("multipart: %q %+v, want ETag %s", body, info, mp.ETag)
	}
	if _, info, ok := e.target("dir/"); !ok || !info.IsDir {
		t.Errorf("directory marker: %+v %v", info, ok)
	}
	if body, _, ok := e.target("dropped"); !ok || body != "outside" {
		t.Errorf("outside file: %q %v", body, ok)
	}
	if copied, _, failed, _, _ := e.r.Stats(); copied != 4 || failed != 0 {
		t.Errorf("copied %d, failed %d", copied, failed)
	}
	if _, ok := e.r.LastCompared()["src/r1"]; !ok {
		t.Error("no comparison time")
	}

	// A second comparison finds nothing to copy.
	e.compareNow(rule)
	if copied, _, _, _, _ := e.r.Stats(); copied != 4 {
		t.Errorf("second comparison copied again: %d", copied)
	}
}

func TestChanges(t *testing.T) {
	e := newEnv(t)
	keep, del := e.rule(), e.rule(func(r *Rule) { r.ID, r.TargetBucket, r.Delete = "mirror", "mirror", true })
	_ = e.dst.CreateBucket("mirror")
	e.r.SetRules([]Rule{keep, del})
	e.waitFor("the first comparisons", func() bool { return e.worker(keep).last.Load() > 0 && e.worker(del).last.Load() > 0 })

	e.put("k", "v1", storage.PutOptions{})
	e.r.Changed("src", "k")
	e.r.Changed("other", "k") // another bucket: no rule
	e.waitFor("the copy", func() bool { body, _, ok := e.target("k"); return ok && body == "v1" })

	// Retagging sends only the tags.
	if err := e.src.SetTags("src", "k", []storage.Tag{{Key: "x", Value: "y"}}); err != nil {
		t.Fatal(err)
	}
	copied, _, _, _, _ := e.r.Stats()
	e.r.Changed("src", "k")
	e.waitFor("the tags", func() bool { _, info, _ := e.target("k"); return len(info.Tags) == 1 })
	if now, _, _, _, _ := e.r.Stats(); now != copied {
		t.Errorf("retagging copied the data again: %d -> %d", copied, now)
	}

	if err := e.src.DeleteObject("src", "k"); err != nil {
		t.Fatal(err)
	}
	e.r.Changed("src", "k")
	e.waitFor("the delete on the mirror", func() bool {
		_, _, ok := e.dst.ReadObject("mirror", "k")
		return errors.Is(ok, storage.ErrNoSuchKey)
	})
	if _, _, ok := e.target("k"); !ok {
		t.Error("a rule without delete removed the target key")
	}
}

func TestComparisonCatchesUp(t *testing.T) {
	e := newEnv(t)
	rule := e.rule(func(r *Rule) { r.Delete, r.Prefix = true, "in/" })
	e.r.SetRules([]Rule{rule})
	e.waitFor("the first comparison", func() bool { return e.worker(rule).last.Load() > 0 })

	e.put("in/missed", "x", storage.PutOptions{}) // no Changed: the queue never heard of it
	e.put("out/ignored", "x", storage.PutOptions{})
	for _, k := range []string{"in/stale", "zz-outside"} {
		if _, err := e.dst.PutObject("dst", k, strings.NewReader("old"), 3, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.compareNow(rule)
	if _, _, ok := e.target("in/missed"); !ok {
		t.Error("missed key not copied")
	}
	if _, _, ok := e.target("in/stale"); ok {
		t.Error("target-only key under the prefix kept with delete")
	}
	for _, k := range []string{"out/ignored", "zz-outside"} {
		if body, _, ok := e.target(k); k == "out/ignored" && ok || k == "zz-outside" && (!ok || body != "old") {
			t.Errorf("%s outside the prefix touched", k)
		}
	}
}

// A target whose ETag differs (another part size, encryption) is matched by the recorded
// source ETag instead, so it is not copied over and over.
func TestRecordedETagMatches(t *testing.T) {
	e := newEnv(t)
	src := e.put("k", "same", storage.PutOptions{})
	if _, err := e.dst.PutObject("dst", "k", strings.NewReader("SAME"), 4, storage.PutOptions{Metadata: storage.Metadata{
		UserMeta: map[string]string{ETagMeta: src.ETag}}}); err != nil {
		t.Fatal(err)
	}
	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	e.waitFor("the first comparison", func() bool { return e.worker(rule).last.Load() > 0 })
	if body, _, _ := e.target("k"); body != "SAME" {
		t.Errorf("copied over a target recorded as current: %q", body)
	}
}

func TestFailures(t *testing.T) {
	e := newEnv(t)
	gone := e.rule(func(r *Rule) { r.ID, r.TargetBucket = "gone", "no-such-bucket" })
	e.put("k", "v", storage.PutOptions{})
	e.r.SetRules([]Rule{gone})
	// The listing fails, so the comparison stops; a change still gets its own attempt,
	// refused for good without retries.
	e.waitFor("the stopped comparison", func() bool { return strings.Contains(e.logs.String(), "replication comparison stopped") })
	e.r.Changed("src", "k")
	e.waitFor("the failure", func() bool { _, _, failed, _, _ := e.r.Stats(); return failed == 1 })
	if !strings.Contains(e.logs.String(), "NoSuchBucket") {
		t.Errorf("failure not logged:\n%s", e.logs.String())
	}

	// A server error is retried; the next attempt goes through.
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 { // the first is the comparison's listing, the second its HEAD
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		e.h.ServeHTTP(w, r)
	}))
	defer flaky.Close()
	retried := e.rule(func(r *Rule) { r.ID, r.Endpoint = "flaky", flaky.URL })
	e.r.SetRules([]Rule{retried})
	e.waitFor("the copy after a retry", func() bool { _, _, ok := e.target("k"); return ok })
}

func TestQueueCoalescesAndOverflows(t *testing.T) {
	e := newEnv(t)
	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	w := e.worker(rule)
	e.waitFor("the first comparison", func() bool { return w.last.Load() > 0 })
	// Stop the worker so the queue fills.
	w.cancel()
	<-w.done
	for range 3 {
		e.r.Changed("src", "hot") // one key waits once
	}
	for i := range queueSize {
		e.r.Changed("src", fmt.Sprintf("k%d", i))
	}
	if _, _, _, dropped, queued := e.r.Stats(); dropped != 1 || queued != queueSize || len(w.kick) != 1 {
		t.Errorf("dropped %d, queued %d, kick %d", dropped, queued, len(w.kick))
	}
}

func TestSetRulesKeepsUnchangedWorkers(t *testing.T) {
	e := newEnv(t)
	a, b := e.rule(), e.rule(func(r *Rule) { r.ID = "r2" })
	e.r.SetRules([]Rule{a, b})
	wa := e.worker(a)
	rotated := a
	rotated.SecretKey = "new"
	e.r.SetRules([]Rule{rotated})
	if e.worker(a) != wa || e.worker(b) != nil {
		t.Error("an unchanged worker was replaced, or a removed one kept")
	}
	if wa.client().Signer.SecretKey != "new" {
		t.Error("the kept worker did not take the new secret")
	}
	select {
	case <-wa.done:
		t.Error("the kept worker stopped")
	default:
	}
}

func TestPartSizes(t *testing.T) {
	if got := partSizes(storage.ObjectInfo{Size: 10, Parts: []storage.PartRecord{{Size: 6}, {Size: 4}}}); len(got) != 2 || got[0] != 6 {
		t.Errorf("recorded parts: %v", got)
	}
	if got := partSizes(storage.ObjectInfo{Size: 11, Parts: []storage.PartRecord{{Size: 6}, {Size: 4}}}); got != nil {
		t.Errorf("parts not adding up: %v", got)
	}
	if got := partSizes(storage.ObjectInfo{Size: 7, Parts: []storage.PartRecord{{Size: 7}}}); len(got) != 1 {
		t.Errorf("a single recorded part: %v", got)
	}
	if got := partSizes(storage.ObjectInfo{Size: maxSinglePut + 1, Parts: []storage.PartRecord{{Size: maxSinglePut + 1}}}); len(got) < 2 {
		t.Errorf("a part too large for S3 kept: %v", got)
	}
	if got := partSizes(storage.ObjectInfo{Size: maxSinglePut}); got != nil {
		t.Errorf("at the single PUT limit: %v", got)
	}
	big := int64(maxSinglePut + 1)
	got := partSizes(storage.ObjectInfo{Size: big})
	var total int64
	for _, n := range got {
		total += n
	}
	if total != big || got[0] != minPartSize || len(got) > maxParts {
		t.Errorf("over the limit: %d parts totalling %d", len(got), total)
	}
	huge := int64(maxParts) * minPartSize * 3
	if got := partSizes(storage.ObjectInfo{Size: huge}); len(got) > maxParts {
		t.Errorf("%d parts for %d bytes", len(got), huge)
	}
}

// Data unchanged, metadata replaced: the target is copied again.
func TestMetadataChange(t *testing.T) {
	e := newEnv(t)
	e.put("k", "same", storage.PutOptions{Metadata: storage.Metadata{ContentType: "text/plain"}})
	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	e.waitFor("the first copy", func() bool { _, _, ok := e.target("k"); return ok })
	for _, md := range []storage.Metadata{{ContentType: "text/html"}, {ContentType: "text/html", UserMeta: map[string]string{"v": "2"}}} {
		e.put("k", "same", storage.PutOptions{Metadata: md})
		e.r.Changed("src", "k")
		e.waitFor("the new metadata", func() bool {
			_, info, _ := e.target("k")
			return info.ContentType == md.ContentType && info.UserMeta["v"] == md.UserMeta["v"]
		})
	}
}

// A part that fails for good abandons the upload instead of leaving it on the target.
func TestFailedPartAborts(t *testing.T) {
	e := newEnv(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("partNumber") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		e.h.ServeHTTP(w, r)
	}))
	defer broken.Close()
	e.multipart("big", "first", "second")
	e.r.SetRules([]Rule{e.rule(func(r *Rule) { r.Endpoint = broken.URL })})
	e.waitFor("the failure", func() bool { _, _, failed, _, _ := e.r.Stats(); return failed == 1 })
	if ups, _, err := e.dst.ListUploads("dst", "", "", "", 10); err != nil || len(ups) != 0 {
		t.Errorf("uploads left on the target: %v %v", ups, err)
	}
}

// Changes that arrive during a long comparison are applied as it goes.
func TestChangesDuringComparison(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"a", "b", "c"} {
		e.put(k, k, storage.PutOptions{})
	}
	release := make(chan struct{})
	var once sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/a") {
			once.Do(func() { <-release })
		}
		e.h.ServeHTTP(w, r)
	}))
	defer slow.Close()
	rule := e.rule(func(r *Rule) { r.Endpoint = slow.URL })
	e.r.SetRules([]Rule{rule})
	e.put("late", "late", storage.PutOptions{})
	e.r.Changed("src", "late")
	close(release)
	e.waitFor("the comparison", func() bool { return e.worker(rule).last.Load() > 0 })
	if _, _, ok := e.target("late"); !ok {
		t.Error("a change queued during the comparison was not applied by its end")
	}
}

func TestPeriodicComparison(t *testing.T) {
	e := newEnv(t)
	e.r.interval = 20 * time.Millisecond
	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	e.waitFor("the first comparison", func() bool { return e.worker(rule).last.Load() > 0 })
	e.put("later", "x", storage.PutOptions{}) // never reported
	e.waitFor("a later comparison to copy it", func() bool { _, _, ok := e.target("later"); return ok })
}

// A source bucket that cannot be read is never taken for an empty one: an unmounted
// volume must not delete the copy.
func TestMissingSourceBucketDeletesNothing(t *testing.T) {
	e := newEnv(t)
	if _, err := e.dst.PutObject("dst", "precious", strings.NewReader("x"), 1, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	rule := e.rule(func(r *Rule) { r.Bucket, r.Delete = "unmounted", true })
	e.r.SetRules([]Rule{rule})
	e.waitFor("the stopped comparison", func() bool { return strings.Contains(e.logs.String(), "replication comparison stopped") })
	e.r.Changed("unmounted", "precious")
	e.waitFor("the failure", func() bool { _, _, failed, _, _ := e.r.Stats(); return failed == 1 })
	if _, _, ok := e.target("precious"); !ok {
		t.Error("the copy was deleted for a source bucket that is not there")
	}
}

// Two servers replicating to each other settle: an unchanged object is not written again.
func TestNoEchoBetweenTwoServers(t *testing.T) {
	e := newEnv(t)
	e.put("k", "v", storage.PutOptions{Metadata: storage.Metadata{Tags: []storage.Tag{{Key: "a", Value: "1"}}}})
	var writes atomic.Int32
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
		}
		e.h.ServeHTTP(w, r)
	}))
	defer counting.Close()
	rule := e.rule(func(r *Rule) { r.Endpoint = counting.URL })
	e.r.SetRules([]Rule{rule})
	e.waitFor("the copy", func() bool { _, info, ok := e.target("k"); return ok && len(info.Tags) == 1 })
	settled := writes.Load()
	for range 3 {
		e.r.Changed("src", "k")
	}
	e.compareNow(rule)
	if n := writes.Load(); n != settled {
		t.Errorf("%d writes after the copy settled", n-settled)
	}
}

// A client's own plainbucket-etag metadata does not replace the recorded source ETag.
func TestClientETagMetaIgnored(t *testing.T) {
	e := newEnv(t)
	info := e.put("k", "v", storage.PutOptions{Metadata: storage.Metadata{UserMeta: map[string]string{ETagMeta: "forged"}}})
	rule := e.rule()
	e.r.SetRules([]Rule{rule})
	// The data file is renamed into place before its sidecar, so wait for the metadata too.
	e.waitFor("the copy", func() bool { _, got, _ := e.target("k"); return got.UserMeta[ETagMeta] != "" })
	if _, got, _ := e.target("k"); got.UserMeta[ETagMeta] != info.ETag {
		t.Errorf("recorded ETag %q, want %q", got.UserMeta[ETagMeta], info.ETag)
	}
}

func TestPermanent(t *testing.T) {
	for err, want := range map[error]bool{
		&s3client.Error{Status: 400, Code: "RequestTimeout"}:  false,
		&s3client.Error{Status: 503, Code: "SlowDown"}:        false,
		&s3client.Error{Status: 501, Code: "NotImplemented"}:  true,
		&s3client.Error{Status: 200, Code: "InvalidPart"}:     true,
		&s3client.Error{Status: 200, Code: "InternalError"}:   false,
		&s3client.Error{Status: 403, Code: "AccessDenied"}:    true,
		&s3client.Error{Status: 429, Code: "TooManyRequests"}: false,
		storage.ErrNoSuchBucket:                               true,
		errors.New("connection reset"):                        false,
	} {
		if permanent(err) != want {
			t.Errorf("permanent(%v) = %v", err, !want)
		}
	}
}
