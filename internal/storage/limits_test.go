package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable time source for the store.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// age sets the data file's mtime and the sidecar's to when, as if written and last read then.
func age(t *testing.T, s *Store, key string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(s.root, "bkt", key), when, when); err != nil {
		t.Fatal(err)
	}
	// The sidecar records the data mtime; refresh it so it stays trusted.
	if _, err := s.HeadObject("bkt", key); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(s.metaPath("bkt", key), when, when)
}

// limitRemovals records what OnLimitRemove reports.
func limitRemovals(s *Store) *[]string {
	var got []string
	s.OnLimitRemove(func(bucket, key string) { got = append(got, bucket+"/"+key) })
	return &got
}

func exists(s *Store, key string) bool {
	_, err := s.HeadObject("bkt", key)
	return err == nil
}

func TestExpireAfterLastUse(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {ExpireAfter: 24 * time.Hour}})
	for _, k := range []string{"old", "read", "fresh"} {
		put(t, s, k, "data")
	}
	twoDays := c.t.Add(-48 * time.Hour)
	age(t, s, "old", twoDays)
	age(t, s, "read", twoDays)
	_ = os.Chtimes(s.metaPath("bkt", "read"), c.t, c.t) // read recently
	removed := limitRemovals(s)
	expired, _ := s.enforceLimits(context.Background())
	if strings.Join(*removed, ",") != "bkt/old" {
		t.Errorf("OnLimitRemove got %v", *removed)
	}
	if expired != 1 || exists(s, "old") || !exists(s, "read") || !exists(s, "fresh") {
		t.Errorf("expired %d; old=%v read=%v fresh=%v", expired, exists(s, "old"), exists(s, "read"), exists(s, "fresh"))
	}
	if e, _ := s.Removed(); e != 1 {
		t.Errorf("Removed expired = %d", e)
	}
}

func TestReadRecordsLastUse(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	put(t, s, "k", "data")
	old := c.t.Add(-3 * time.Hour)
	age(t, s, "k", old)
	f, _, err := s.GetObject("bkt", "k")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	fi, _ := os.Stat(s.metaPath("bkt", "k"))
	if !fi.ModTime().Equal(c.t) {
		t.Errorf("sidecar mtime %v, want %v", fi.ModTime(), c.t)
	}
	// Within touchInterval a read writes nothing.
	c.t = c.t.Add(10 * time.Minute)
	f, _, _ = s.GetObject("bkt", "k")
	_ = f.Close()
	if fi2, _ := os.Stat(s.metaPath("bkt", "k")); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("sidecar touched again within the interval")
	}
	// The sidecar stays trusted: no re-hash, metadata kept.
	if info, err := s.HeadObject("bkt", "k"); err != nil || info.ETag != md5ETag(md5sum("data")) {
		t.Errorf("after touch: %+v %v", info, err)
	}
}

func TestEvictLeastRecentlyUsed(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 10, Evict: true}})
	put(t, s, "a", "aaaa")
	put(t, s, "b", "bbbb")
	age(t, s, "a", c.t.Add(-3*time.Hour))
	age(t, s, "b", c.t.Add(-2*time.Hour))
	// Over the limit: the write is accepted and wakes the janitor.
	put(t, s, "c", "cccc")
	select {
	case <-s.kick:
	default:
		t.Fatal("janitor not woken")
	}
	removed := limitRemovals(s)
	_, evicted := s.enforceLimits(context.Background())
	if strings.Join(*removed, ",") != "bkt/a" {
		t.Errorf("OnLimitRemove got %v", *removed)
	}
	if evicted != 1 || exists(s, "a") || !exists(s, "b") || !exists(s, "c") {
		t.Errorf("evicted %d; a=%v b=%v c=%v", evicted, exists(s, "a"), exists(s, "b"), exists(s, "c"))
	}
	if u := s.BucketUsage()["bkt"]; u.Bytes != 8 || u.Objects != 2 {
		t.Errorf("usage after eviction = %+v", u)
	}
}

func TestQuotaRefusesWrites(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 10}})
	put(t, s, "a", "123456")
	if _, err := s.PutObject("bkt", "b", strings.NewReader("12345"), 5, PutOptions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("over quota: %v", err)
	}
	// Replacing an object counts only the difference.
	if _, err := s.PutObject("bkt", "a", strings.NewReader("1234567890"), 10, PutOptions{}); err != nil {
		t.Errorf("replace within quota: %v", err)
	}
	if _, err := s.CopyObject(context.Background(), "bkt", "a", "bkt", "copy", CopyOptions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("copy over quota: %v", err)
	}
	if err := s.DeleteObject("bkt", "a"); err != nil {
		t.Fatal(err)
	}
	if u := s.BucketUsage()["bkt"]; u.Bytes != 0 || u.Objects != 0 {
		t.Errorf("usage after delete = %+v", u)
	}
	s.MinPartSize = 1
	id, _ := s.CreateUpload("bkt", "mp", Metadata{}, MultipartChecksum{})
	p1, _ := s.PutPart("bkt", "mp", id, 1, strings.NewReader("123456"), 6, nil)
	p2, _ := s.PutPart("bkt", "mp", id, 2, strings.NewReader("123456"), 6, nil)
	if _, err := s.CompleteUpload(context.Background(), "bkt", "mp", id, []CompletePart{{1, p1.ETag}, {2, p2.ETag}}, Conditions{}, 0, CompleteChecksums{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("multipart over quota: %v", err)
	}
	// The commit-time check catches what the early one cannot see.
	s.usageMu.Lock()
	s.bucketUsage["bkt"] = Usage{Bytes: 0}
	s.usageMu.Unlock()
	tmp, sum, _, err := s.receive(strings.NewReader("12345678901"), 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.commit("bkt", "late", tmp, sidecar{ETag: md5ETag(sum)}, Conditions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("commit over quota: %v", err)
	}
	if exists(s, "late") {
		t.Error("refused object was stored")
	}
}

func TestRewrittenObjectSurvives(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {ExpireAfter: time.Hour}})
	put(t, s, "k", "old")
	age(t, s, "k", c.t.Add(-2*time.Hour))
	cands := s.scanBucket(context.Background(), "bkt")
	put(t, s, "k", "new data")
	if s.removeIfUnused("bkt", cands[0], c.t.Add(-time.Hour)) || !exists(s, "k") {
		t.Error("an object rewritten after the scan was removed")
	}
}

func TestMeasureUsagePerBucket(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 100}, "none": {MaxSize: 1}})
	put(t, s, "a", "123")
	_ = os.WriteFile(filepath.Join(s.root, "bkt", "external"), []byte("12345"), 0o644)
	s.MeasureUsage(context.Background())
	u := s.BucketUsage()
	if u["bkt"].Bytes != 8 || u["bkt"].Objects != 2 || u["none"].Bytes != 0 {
		t.Errorf("usage = %+v", u)
	}
	if err := s.CreateBucket("free"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("free", "x", strings.NewReader("xy"), 2, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	s.MeasureUsage(context.Background())
	if all := s.AllBucketUsage(); all["free"].Bytes != 2 || all["bkt"].Bytes != 8 || all["free"].At.IsZero() {
		t.Errorf("all buckets = %+v", all)
	}
	if _, ok := s.BucketUsage()["free"]; ok {
		t.Error("BucketUsage lists a bucket without limits")
	}
	if l := s.BucketLimit("bkt"); l.MaxSize != 100 || !reflect.DeepEqual(s.BucketLimit("free"), Limit{}) {
		t.Errorf("limits = %+v", l)
	}
	if err := s.CreateBucket("gone"); err != nil {
		t.Fatal(err)
	}
	put2 := func() { _, _ = s.PutObject("gone", "x", strings.NewReader("x"), 1, PutOptions{}) }
	put2()
	_ = s.DeleteObject("gone", "x")
	if err := s.DeleteBucket("gone"); err != nil {
		t.Fatal(err)
	}
	s.usageMu.Lock()
	_, kept := s.bucketUsage["gone"]
	s.usageMu.Unlock()
	if kept {
		t.Error("usage of a deleted bucket kept")
	}
}

func TestJanitorAppliesLimitsWhenWoken(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 4, Evict: true}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunJanitor(ctx, time.Hour, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	// Write after the startup measurement, which would otherwise overwrite the tracked usage.
	for s.Usage().At.IsZero() {
		time.Sleep(time.Millisecond)
	}
	put(t, s, "a", "aaaa")
	_ = os.Chtimes(filepath.Join(s.root, "bkt", "a"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	_ = os.Remove(s.metaPath("bkt", "a"))
	put(t, s, "b", "bbbb")
	// Poll the file, not HeadObject: a read would record a use of "a" and change the order.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(s.root, "bkt", "a")); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if exists(s, "a") || !exists(s, "b") {
		t.Errorf("a=%v b=%v", exists(s, "a"), exists(s, "b"))
	}
}

func TestEvictRefusesAnObjectLargerThanTheBucket(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 4, Evict: true}})
	if _, err := s.PutObject("bkt", "big", strings.NewReader("12345"), 5, PutOptions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("object over the whole limit: %v", err)
	}
}

func TestShortExpiryTouchesMoreOften(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {ExpireAfter: 20 * time.Minute}})
	put(t, s, "k", "data")
	age(t, s, "k", c.t.Add(-6*time.Minute))
	f, _, _ := s.GetObject("bkt", "k")
	_ = f.Close()
	if fi, _ := os.Stat(s.metaPath("bkt", "k")); !fi.ModTime().Equal(c.t) {
		t.Errorf("a read 6m after the last use of a 20m-expiry object was not recorded: %v", fi.ModTime())
	}
}

func TestChangesDuringMeasurementAreKept(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 100}})
	s.usageMu.Lock()
	s.measuring = map[string]Usage{}
	s.usageMu.Unlock()
	// A write the walk has not seen yet...
	if _, err := s.account("bkt", 1, 7); err != nil {
		t.Fatal(err)
	}
	s.adjust("bkt", 0, -2)
	s.usageMu.Lock()
	d := s.measuring["bkt"]
	s.measuring = nil
	s.usageMu.Unlock()
	if d.Objects != 1 || d.Bytes != 5 {
		t.Errorf("recorded during measurement = %+v", d)
	}
	put(t, s, "a", "123")
	s.MeasureUsage(context.Background())
	if u := s.BucketUsage()["bkt"]; u.Bytes != 3 {
		t.Errorf("after measurement = %+v", u)
	}
}

func TestEvictionKeepsObjectReadAfterScan(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	put(t, s, "k", "data")
	age(t, s, "k", c.t.Add(-2*time.Hour))
	cands := s.scanBucket(context.Background(), "bkt")
	_ = os.Chtimes(s.metaPath("bkt", "k"), c.t, c.t) // read after the scan
	if s.removeIfUnused("bkt", cands[0], cands[0].used.Add(time.Nanosecond)) {
		t.Error("an object read after the scan was evicted")
	}
}

func TestPutUnknownSize(t *testing.T) {
	s := newStore(t)
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 10}})
	info, err := s.PutObject("bkt", "a", strings.NewReader("123456"), -1, PutOptions{})
	if err != nil || info.Size != 6 {
		t.Fatalf("unknown size: %+v %v", info, err)
	}
	if _, err := s.PutObject("bkt", "b", strings.NewReader("12345"), -1, PutOptions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("over quota once received: %v", err)
	}
	if exists(s, "b") {
		t.Error("refused object stored")
	}
	if _, err := s.PutObject("bkt", "dir/", strings.NewReader("x"), -1, PutOptions{}); !errors.Is(err, ErrMarkerNotEmpty) {
		t.Errorf("marker with a body: %v", err)
	}
	if _, err := s.PutObject("bkt", "dir/", strings.NewReader(""), -1, PutOptions{}); err != nil {
		t.Errorf("empty marker: %v", err)
	}
}

func TestPausedJanitorRemovesNothing(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {ExpireAfter: time.Hour}})
	put(t, s, "old", "data")
	age(t, s, "old", c.t.Add(-2*time.Hour))
	stale := filepath.Join(s.sys("tmp"), "stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(stale, c.t.Add(-2*staleTmpAge), c.t.Add(-2*staleTmpAge))
	// A HeadObject would count as a read and keep the object from expiring.
	onDisk := func() bool { _, err := os.Stat(filepath.Join(s.root, "bkt", "old")); return err == nil }
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	s.SetPaused(true)
	s.applyLimits(ctx, log)
	if n := s.sweepUnlessPaused(ctx, time.Hour); n != 0 || !onDisk() {
		t.Fatalf("paused janitor removed %d, old kept %v", n, onDisk())
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("paused sweep removed a staging file")
	}

	// A pass already under way removes nothing once paused, nor does a direct sweep.
	fi, _ := os.Stat(filepath.Join(s.root, "bkt", "old"))
	if s.removeIfUnused("bkt", candidate{key: "old", size: fi.Size(), mtimeNs: fi.ModTime().UnixNano()}, c.t) || !onDisk() {
		t.Fatal("removeIfUnused removed an object while paused")
	}
	if n := s.Sweep(ctx, time.Hour); n != 0 {
		t.Fatalf("paused Sweep removed %d", n)
	}

	s.SetPaused(false)
	select {
	case <-s.kick:
	default:
		t.Error("resuming did not wake the janitor")
	}
	s.applyLimits(ctx, log)
	if n := s.sweepUnlessPaused(ctx, time.Hour); n == 0 || onDisk() {
		t.Errorf("resumed janitor removed %d, old kept %v", n, onDisk())
	}
}

func TestLifecycleRules(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	l := Limit{ExpireAfter: 48 * time.Hour, Rules: []LifecycleRule{
		{Prefix: "logs/", DeleteAfter: 24 * time.Hour},
		{Prefix: "logs/keep/"},
		{Prefix: "tmp/", ExpireAfter: time.Hour},
	}}
	s.SetLimits(map[string]Limit{"bkt": l})
	keys := map[string]time.Duration{ // how long ago each was written and last read
		"logs/old": 48 * time.Hour, "logs/new": time.Hour, "logs/keep/x": 240 * time.Hour,
		"tmp/stale": 2 * time.Hour, "tmp/warm": 30 * time.Minute, "other": 72 * time.Hour, "fresh": 0,
	}
	for k, ago := range keys {
		put(t, s, k, "data")
		age(t, s, k, c.t.Add(-ago))
	}
	// Read just now: delete-after still counts from the write.
	_ = os.Chtimes(s.metaPath("bkt", "logs/old"), c.t, c.t)
	onDisk := func(k string) bool { _, err := os.Stat(filepath.Join(s.root, "bkt", k)); return err == nil }

	removed := limitRemovals(s)
	expired, _ := s.enforceLimits(context.Background())
	gone := strings.Join(slices.Sorted(slices.Values(*removed)), ",")
	if expired != 3 || gone != "bkt/logs/old,bkt/other,bkt/tmp/stale" {
		t.Errorf("expired %d: %s", expired, gone)
	}
	for _, k := range []string{"logs/new", "logs/keep/x", "tmp/warm", "fresh"} {
		if !onDisk(k) {
			t.Errorf("%s removed", k)
		}
	}

	if e, d := l.ruleFor("logs/keep/deep/y"); e != 0 || d != 0 {
		t.Errorf("exempt prefix: %v %v", e, d)
	}
	if e, d := l.ruleFor("elsewhere"); e != 48*time.Hour || d != 0 {
		t.Errorf("bucket default: %v %v", e, d)
	}
	if (Limit{Rules: []LifecycleRule{{Prefix: "keep/"}}}).expires() {
		t.Error("a rule that keeps everything expires")
	}
}

func TestLifecycleBothAndRewrite(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	s.SetLimits(map[string]Limit{"bkt": {Rules: []LifecycleRule{{Prefix: "b/", ExpireAfter: time.Hour, DeleteAfter: 24 * time.Hour}}}})
	put(t, s, "b/old-read", "data")
	age(t, s, "b/old-read", c.t.Add(-25*time.Hour))
	_ = os.Chtimes(s.metaPath("bkt", "b/old-read"), c.t.Add(-time.Minute), c.t.Add(-time.Minute))
	put(t, s, "b/unread", "data")
	age(t, s, "b/unread", c.t.Add(-2*time.Hour))
	put(t, s, "b/read", "data")
	age(t, s, "b/read", c.t.Add(-2*time.Hour))
	_ = os.Chtimes(s.metaPath("bkt", "b/read"), c.t.Add(-10*time.Minute), c.t.Add(-10*time.Minute))
	onDisk := func(k string) bool { _, err := os.Stat(filepath.Join(s.root, "bkt", k)); return err == nil }

	// Whichever of the two comes first removes it, and expiry never goes to the trash.
	if expired, _ := s.enforceLimits(context.Background()); expired != 2 || onDisk("b/old-read") || onDisk("b/unread") || !onDisk("b/read") {
		t.Errorf("expired %d; old-read %v unread %v read %v", expired, onDisk("b/old-read"), onDisk("b/unread"), onDisk("b/read"))
	}
	if list, _ := s.ListTrash("bkt"); len(list) != 0 {
		t.Errorf("expired objects in the trash: %+v", list)
	}

	// A same-size rewrite after the scan has a new mtime, so delete-after keeps it.
	put(t, s, "b/k", "old1")
	age(t, s, "b/k", c.t.Add(-48*time.Hour))
	var cand candidate
	for _, x := range s.scanBucket(context.Background(), "bkt") {
		if x.key == "b/k" {
			cand = x
		}
	}
	put(t, s, "b/k", "new1")
	if s.removeUnchanged("bkt", cand, nil) || !onDisk("b/k") {
		t.Error("removed an object rewritten after the scan")
	}
}

func TestLifecycleExemptStillEvicts(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {MaxSize: 6, Evict: true, Rules: []LifecycleRule{{Prefix: "keep/"}}}})
	put(t, s, "keep/a", "aaaa")
	age(t, s, "keep/a", c.t.Add(-time.Hour))
	put(t, s, "b", "bbbb")
	if _, evicted := s.enforceLimits(context.Background()); evicted != 1 || exists(s, "keep/a") {
		t.Errorf("evicted %d; keep/a kept %v", evicted, exists(s, "keep/a"))
	}
}

func TestLifecycleRuleTouchesMoreOften(t *testing.T) {
	s := newStore(t)
	c := &clock{t: time.Now()}
	s.now = c.now
	s.SetLimits(map[string]Limit{"bkt": {Rules: []LifecycleRule{{Prefix: "t/", ExpireAfter: 20 * time.Minute}}}})
	for _, k := range []string{"t/k", "other"} {
		put(t, s, k, "data")
		age(t, s, k, c.t)
	}
	c.t = c.t.Add(6 * time.Minute)
	for _, k := range []string{"t/k", "other"} {
		f, _, err := s.GetObject("bkt", k)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	// A quarter of the rule's 20m for its prefix; the hourly default elsewhere.
	if fi, _ := os.Stat(s.metaPath("bkt", "t/k")); !fi.ModTime().Equal(c.t) {
		t.Error("read under a short rule not recorded")
	}
	if fi, _ := os.Stat(s.metaPath("bkt", "other")); fi.ModTime().Equal(c.t) {
		t.Error("read outside the rule recorded early")
	}
}

// A bucket created or deleted while a measurement walks keeps only what happened after
// that: it neither disappears, comes back, nor carries the deletes made before it.
func TestMeasureUsageBucketChurn(t *testing.T) {
	s := newStore(t)
	for _, b := range []string{"gone", "old"} {
		if err := s.CreateBucket(b); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"x", "y"} {
			if _, err := s.PutObject(b, k, strings.NewReader("12"), 2, PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Store.now runs after each bucket's walk: the first call is after "bkt", before the others.
	var fired atomic.Bool // not sync.Once: the deletes below call Store.now again
	now := s.now
	s.now = func() time.Time {
		if fired.CompareAndSwap(false, true) {
			for _, b := range []string{"gone", "old"} {
				for _, k := range []string{"x", "y"} {
					if err := s.DeleteObject(b, k); err != nil {
						t.Error(err)
					}
				}
				if err := s.DeleteBucket(b); err != nil {
					t.Error(err)
				}
			}
			for _, b := range []string{"gone", "late"} {
				if err := s.CreateBucket(b); err != nil {
					t.Error(err)
				}
			}
		}
		return now()
	}
	s.MeasureUsage(context.Background())
	all := s.AllBucketUsage()
	if _, ok := all["old"]; ok {
		t.Errorf("a deleted bucket came back: %+v", all)
	}
	for _, b := range []string{"gone", "late"} {
		if u, ok := all[b]; !ok || u.Objects != 0 || u.Bytes != 0 {
			t.Errorf("%s = %+v, %v", b, u, ok)
		}
	}
}
