// Package replicate copies a bucket's objects to another S3 endpoint. Each rule has one
// goroutine that both follows the changes the server reports and, periodically, compares
// the two listings to catch up on what the queue lost: a full queue, a failed request, a
// restart, or objects that predate the rule. Every difference is resolved by making the
// target key match the source key, except that a target key with no source is removed
// only by a rule with Delete.
package replicate

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/s3client"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

// Rule replicates the keys of Bucket under Prefix to TargetBucket at Endpoint.
type Rule struct {
	ID, Bucket, Prefix string
	Endpoint, Region   string
	VirtualHosted      bool
	TargetBucket       string
	AccessKey          string
	SecretKey          string
	// Delete removes target keys whose source key is gone; otherwise they are kept.
	Delete bool
}

// identity is the rule without its credentials: a reload that only rotates the key keeps
// the worker, its queue and its connections.
func (r Rule) identity() Rule {
	r.AccessKey, r.SecretKey = "", ""
	return r
}

// Source is the store objects are read from.
type Source interface {
	ReadObject(bucket, key string) (*os.File, storage.ObjectInfo, error)
	List(bucket string, p storage.ListParams) (storage.ListResult, error)
}

// ETagMeta is the user metadata key that records the source ETag on a target object,
// for targets whose own ETag differs (another part size, or encryption at rest).
const ETagMeta = "plainbucket-etag"

const (
	queueSize = 1000 // distinct keys waiting per rule
	listPage  = 1000
	// maxSinglePut is S3's limit for one PUT and for one part.
	maxSinglePut = 5 << 30
	minPartSize  = 64 << 20
	maxParts     = 10000
	abortTimeout = 15 * time.Second
	// minRate bounds how long one request may take: a minute plus a second per MiB.
	minRate = 1 << 20
)

var defaultBackoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// Replicator runs the rules.
type Replicator struct {
	src      Source
	log      *slog.Logger
	http     *http.Client
	interval time.Duration // between comparisons of the listings; 0: only at start and after a dropped change

	mu      sync.Mutex
	workers map[Rule]*worker // by identity
	list    atomic.Pointer[[]*worker]

	copied, deleted, failed, dropped atomic.Int64
	now                              func() time.Time
	backoff                          []time.Duration
}

type worker struct {
	rule   Rule // identity
	signer atomic.Pointer[sigv4.Signer]
	base   s3client.Client

	qmu     sync.Mutex
	pending map[string]bool // keys waiting, each once
	order   []string
	ready   chan struct{} // signalled when pending gains a key
	kick    chan struct{} // compare now

	cancel context.CancelFunc
	done   chan struct{}
	last   atomic.Int64 // unix time the last comparison finished
}

// New returns a Replicator that compares the listings every interval.
func New(src Source, interval time.Duration, log *slog.Logger) *Replicator {
	r := &Replicator{src: src, log: log, interval: interval, workers: map[Rule]*worker{}, now: time.Now, backoff: defaultBackoff,
		http: &http.Client{
			// A redirect would be replayed without its signature; report it instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ResponseHeaderTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4,
			},
		}}
	r.list.Store(&[]*worker{})
	return r
}

// SetRules replaces the rules. Workers of unchanged rules keep running, taking new
// credentials if those changed; removed ones stop, abandoning what they were copying,
// before new ones start with a comparison.
func (r *Replicator) SetRules(rules []Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[Rule]Rule{}
	for _, rule := range rules {
		next[rule.identity()] = rule
	}
	var stopping []*worker
	// A changed rule keeps its last comparison time, so a staleness alert does not lose it.
	last := map[string]int64{}
	for id, w := range r.workers {
		if _, ok := next[id]; !ok {
			w.cancel()
			stopping = append(stopping, w)
			delete(r.workers, id)
		}
	}
	for _, w := range stopping {
		<-w.done // read after it exits: a comparison may finish while it stops
		last[w.rule.Bucket+"/"+w.rule.ID] = w.last.Load()
	}
	for id, rule := range next {
		if w, ok := r.workers[id]; ok {
			w.signer.Store(signerOf(rule))
			continue
		}
		r.workers[id] = r.start(rule, last[rule.Bucket+"/"+rule.ID])
	}
	list := slices.Collect(maps.Values(r.workers))
	r.list.Store(&list)
}

func signerOf(rule Rule) *sigv4.Signer {
	return &sigv4.Signer{AccessKey: rule.AccessKey, SecretKey: rule.SecretKey, Region: rule.Region}
}

func (r *Replicator) start(rule Rule, last int64) *worker {
	u, _ := url.Parse(rule.Endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{rule: rule.identity(), pending: map[string]bool{}, ready: make(chan struct{}, 1), kick: make(chan struct{}, 1),
		cancel: cancel, done: make(chan struct{}),
		base: s3client.Client{Endpoint: u, VirtualHosted: rule.VirtualHosted, HTTP: r.http, Now: r.now}}
	w.signer.Store(signerOf(rule))
	w.last.Store(last)
	w.kick <- struct{}{} // compare once at start
	go r.run(ctx, w)
	return w
}

// client returns a client signing with the rule's current credentials.
func (w *worker) client() *s3client.Client {
	c := w.base
	c.Signer = *w.signer.Load()
	return &c
}

// Changed reports that bucket/key was written, deleted or retagged. It never blocks: a key
// already waiting is not queued twice, and a change that does not fit is left to the next
// comparison, brought forward.
func (r *Replicator) Changed(bucket, key string) {
	if r == nil {
		return
	}
	for _, w := range *r.list.Load() {
		if w.rule.Bucket != bucket || !strings.HasPrefix(key, w.rule.Prefix) {
			continue
		}
		w.qmu.Lock()
		switch {
		case w.pending[key]:
		case len(w.pending) >= queueSize:
			r.dropped.Add(1)
			signal(w.kick)
		default:
			w.pending[key] = true
			w.order = append(w.order, key)
			signal(w.ready)
		}
		w.qmu.Unlock()
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// next takes the oldest waiting key.
func (w *worker) next() (string, bool) {
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if len(w.order) == 0 {
		return "", false
	}
	key := w.order[0]
	w.order = w.order[1:]
	delete(w.pending, key)
	return key, true
}

func (w *worker) queued() int {
	w.qmu.Lock()
	defer w.qmu.Unlock()
	return len(w.pending)
}

// Stats returns the keys copied, target keys deleted, keys given up on after retries,
// changes dropped from a full queue, and the keys queued now.
func (r *Replicator) Stats() (copied, deleted, failed, dropped int64, queued int) {
	if r == nil {
		return
	}
	for _, w := range *r.list.Load() {
		queued += w.queued()
	}
	return r.copied.Load(), r.deleted.Load(), r.failed.Load(), r.dropped.Load(), queued
}

// LastCompared returns when each rule's last comparison of the listings finished, keyed
// by "<bucket>/<rule id>".
func (r *Replicator) LastCompared() map[string]time.Time {
	out := map[string]time.Time{}
	if r == nil {
		return out
	}
	for _, w := range *r.list.Load() {
		if t := w.last.Load(); t > 0 {
			out[w.rule.Bucket+"/"+w.rule.ID] = time.Unix(t, 0)
		}
	}
	return out
}

// Rules returns how many rules are running.
func (r *Replicator) Rules() int {
	if r == nil {
		return 0
	}
	return len(*r.list.Load())
}

// Close stops every worker, abandoning queued changes; the next start compares again.
func (r *Replicator) Close() {
	r.SetRules(nil)
	r.http.CloseIdleConnections()
}

func (r *Replicator) run(ctx context.Context, w *worker) {
	defer close(w.done)
	var tick <-chan time.Time
	var timer *time.Timer
	if r.interval > 0 {
		timer = time.NewTimer(r.interval)
		defer timer.Stop()
		tick = timer.C
	}
	compare := func() {
		r.compare(ctx, w)
		if timer != nil { // the interval runs from the end of a comparison
			timer.Reset(r.interval)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.ready:
			r.drain(ctx, w)
		case <-w.kick:
			compare()
		case <-tick:
			compare()
		}
	}
}

// drain applies the waiting changes, so a long comparison does not hold them up.
func (r *Replicator) drain(ctx context.Context, w *worker) {
	for ctx.Err() == nil {
		key, ok := w.next()
		if !ok {
			return
		}
		r.syncWithRetry(ctx, w, key)
	}
}

func (r *Replicator) syncWithRetry(ctx context.Context, w *worker, key string) {
	err := retry(ctx, r.backoff, func() error { return r.sync(ctx, w, key) })
	if err != nil && ctx.Err() == nil {
		r.failed.Add(1)
		r.log.Warn("replication failed; the next comparison retries it", "rule", w.rule.ID, "bucket", w.rule.Bucket, "key", key, "err", err)
	}
}

// partGaveUp is a part that exhausted its own retries. Retrying the upload would resend
// every earlier part, so the key waits for the next comparison.
type partGaveUp struct{ err error }

func (e partGaveUp) Error() string { return e.err.Error() }
func (e partGaveUp) Unwrap() error { return e.err }

// retry runs fn until it succeeds, fails for good, or runs out of delays.
func retry(ctx context.Context, delays []time.Duration, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || ctx.Err() != nil || attempt == len(delays) || permanent(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
}

// permanent reports an error not worth repeating the request for: it will not fix it, or
// (partGaveUp) the part already had its retries.
func permanent(err error) bool {
	var gaveUp partGaveUp
	if errors.Is(err, storage.ErrNoSuchBucket) || errors.As(err, &gaveUp) {
		return true
	}
	var e *s3client.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case "RequestTimeout", "SlowDown", "InternalError", "ServiceUnavailable":
		return false
	case "NotImplemented", "EntityTooSmall", "EntityTooLarge", "InvalidPart", "InvalidPartOrder", "MalformedXML":
		return true
	}
	return e.Status == http.StatusNotImplemented ||
		e.Status >= 400 && e.Status < 500 && e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}

// deadline bounds one request carrying n bytes.
func deadline(ctx context.Context, n int64) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Minute+time.Duration(n/minRate)*time.Second)
}

// sync makes the target key match the source key.
func (r *Replicator) sync(ctx context.Context, w *worker, key string) error {
	f, info, err := r.src.ReadObject(w.rule.Bucket, key)
	switch {
	case errors.Is(err, storage.ErrNoSuchBucket):
		// Never a reason to delete: an unmounted volume or an I/O error looks the same.
		return fmt.Errorf("source bucket %s: %w", w.rule.Bucket, err)
	case errors.Is(err, storage.ErrNoSuchKey):
		if !w.rule.Delete {
			return nil
		}
		dctx, cancel := deadline(ctx, 0)
		defer cancel()
		if err := w.client().DeleteObject(dctx, w.rule.TargetBucket, key); err != nil {
			return err
		}
		r.deleted.Add(1)
		return nil
	case err != nil:
		return err
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	c := w.client()
	meta := targetMeta(info)
	hctx, cancel := deadline(ctx, 0)
	defer cancel()
	tgt, err := c.HeadObject(hctx, w.rule.TargetBucket, key)
	switch {
	case err == nil && same(info, meta, tgt):
		// HEAD does not show tags; they are read, and written only when they differ, so two
		// servers replicating to each other do not echo tag writes forever.
		cur, err := c.GetObjectTagging(hctx, w.rule.TargetBucket, key)
		if err != nil || equalTags(cur, meta.Tags) {
			return err
		}
		return c.PutObjectTagging(hctx, w.rule.TargetBucket, key, meta.Tags)
	case err != nil && !s3client.IsNotFound(err):
		return err
	}
	if err := r.upload(ctx, c, w, key, f, info, meta); err != nil {
		return err
	}
	r.copied.Add(1)
	return nil
}

func targetMeta(info storage.ObjectInfo) s3client.Meta {
	m := s3client.Meta{ContentType: info.ContentType, Headers: info.Headers, UserMeta: maps.Clone(info.UserMeta)}
	if m.UserMeta == nil {
		m.UserMeta = map[string]string{}
	}
	m.UserMeta[ETagMeta] = info.ETag // set last: a client's own value must not win
	for _, t := range info.Tags {
		m.Tags = append(m.Tags, s3client.Tag{Key: t.Key, Value: t.Value})
	}
	return m
}

func equalTags(a, b []s3client.Tag) bool {
	key := func(x, y s3client.Tag) int { return strings.Compare(x.Key, y.Key) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, key)
	slices.SortFunc(b, key)
	return slices.Equal(a, b)
}

// same reports whether the target holds this version of the source object.
func same(info storage.ObjectInfo, meta s3client.Meta, tgt s3client.Object) bool {
	if tgt.Size != info.Size || (tgt.ETag != info.ETag && tgt.Meta.UserMeta[ETagMeta] != info.ETag) {
		return false
	}
	if tgt.Meta.ContentType != meta.ContentType && meta.ContentType != "" {
		return false
	}
	for k, v := range meta.UserMeta {
		// The target's value comes decoded, so a source value that is itself an encoded word
		// is compared decoded too; otherwise it would be copied again at every comparison.
		if t := tgt.Meta.UserMeta[strings.ToLower(k)]; t != v && t != s3client.DecodeMeta(v) {
			return false
		}
	}
	for _, k := range httpx.StoredHeaders {
		if tgt.Meta.Headers[k] != meta.Headers[k] {
			return false
		}
	}
	return true
}

// upload copies the object, in the parts it was assembled from when that is possible, so
// the target computes the same ETag. Every piece carries its MD5 for the target to check,
// read from the source ETag when that is one and computed otherwise; each part is retried
// on its own.
func (r *Replicator) upload(ctx context.Context, c *s3client.Client, w *worker, key string, f *os.File, info storage.ObjectInfo, meta s3client.Meta) error {
	if f == nil {
		pctx, cancel := deadline(ctx, 0)
		defer cancel()
		_, err := c.PutObject(pctx, w.rule.TargetBucket, key, strings.NewReader(""), 0, nil, meta)
		return err
	}
	sizes := partSizes(info)
	if len(sizes) == 0 {
		sum := s3client.MD5(info.ETag)
		if sum == nil {
			var err error
			if sum, err = md5Of(io.NewSectionReader(f, 0, info.Size)); err != nil {
				return err
			}
		}
		pctx, cancel := deadline(ctx, info.Size)
		defer cancel()
		_, err := c.PutObject(pctx, w.rule.TargetBucket, key, io.NewSectionReader(f, 0, info.Size), info.Size, sum, meta)
		return err
	}
	cctx, cancel := deadline(ctx, 0)
	id, err := c.CreateMultipartUpload(cctx, w.rule.TargetBucket, key, meta)
	cancel()
	if err != nil {
		return err
	}
	abort := func() {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
		defer cancel()
		_ = c.AbortMultipartUpload(actx, w.rule.TargetBucket, key, id)
	}
	parts := make([]s3client.Part, len(sizes))
	var off int64
	for i, n := range sizes {
		sum, err := md5Of(io.NewSectionReader(f, off, n))
		if err == nil {
			err = retry(ctx, r.backoff, func() error {
				pctx, cancel := deadline(ctx, n)
				defer cancel()
				var err error
				parts[i].ETag, err = c.UploadPart(pctx, w.rule.TargetBucket, key, id, i+1, io.NewSectionReader(f, off, n), n, sum)
				return err
			})
			if err != nil {
				err = partGaveUp{err}
			}
		}
		if err != nil {
			abort()
			return err
		}
		parts[i].Number = i + 1
		off += n
	}
	dctx, cancel := deadline(ctx, 0)
	defer cancel()
	if _, err := c.CompleteMultipartUpload(dctx, w.rule.TargetBucket, key, id, parts); err != nil {
		abort()
		return err
	}
	return nil
}

func md5Of(r io.Reader) ([]byte, error) {
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// partSizes returns the part sizes to upload in, or none for a single PUT: the recorded
// parts when the target accepts them, else even parts for an object too large for one PUT.
func partSizes(info storage.ObjectInfo) []int64 {
	if len(info.Parts) > 0 {
		var sizes []int64
		var total int64
		fits := true
		for _, p := range info.Parts {
			sizes = append(sizes, p.Size)
			total += p.Size
			fits = fits && p.Size <= maxSinglePut
		}
		if fits && total == info.Size && len(sizes) <= maxParts {
			return sizes
		}
	}
	if info.Size <= maxSinglePut {
		return nil
	}
	size := max(minPartSize, int64(math.Ceil(float64(info.Size)/maxParts)))
	var sizes []int64
	for left := info.Size; left > 0; left -= size {
		sizes = append(sizes, min(size, left))
	}
	return sizes
}

// compare walks both listings in key order and syncs every key that differs, applying
// waiting changes after each page.
func (r *Replicator) compare(ctx context.Context, w *worker) {
	next, stop := iter.Pull2(r.targetKeys(ctx, w))
	defer stop()
	tgt, tgtErr, more := next()
	advance := func() { tgt, tgtErr, more = next() }
	before := r.copied.Load() + r.deleted.Load()
	stopped := func(err error) {
		if ctx.Err() == nil {
			r.log.Warn("replication comparison stopped", "rule", w.rule.ID, "bucket", w.rule.Bucket, "err", err)
		}
	}
	after := ""
	for {
		page, err := r.src.List(w.rule.Bucket, storage.ListParams{Prefix: w.rule.Prefix, After: after, MaxKeys: listPage})
		if err != nil {
			// A missing source bucket stops it too: an unmounted root looks the same, and
			// treating it as empty would delete the whole target under Delete.
			stopped(err)
			return
		}
		for _, o := range page.Objects {
			for more && tgtErr == nil && tgt.Key < o.Key {
				if w.rule.Delete {
					r.syncWithRetry(ctx, w, tgt.Key) // only on the target
				}
				advance()
			}
			if tgtErr != nil {
				stopped(tgtErr)
				return
			}
			if more && tgt.Key == o.Key {
				if tgt.Size != o.Size || tgt.ETag != o.ETag {
					r.syncWithRetry(ctx, w, o.Key)
				}
				advance()
			} else {
				r.syncWithRetry(ctx, w, o.Key) // only on the source
			}
			if ctx.Err() != nil {
				return
			}
		}
		r.drain(ctx, w)
		if !page.Truncated {
			break
		}
		after = page.Next
	}
	for ; more && tgtErr == nil && w.rule.Delete; advance() {
		r.syncWithRetry(ctx, w, tgt.Key)
	}
	if tgtErr != nil {
		stopped(tgtErr)
		return
	}
	w.last.Store(r.now().Unix())
	// From the totals, so it includes changes applied meanwhile and other rules' work,
	// but not keys whose target only lists another ETag and turned out current.
	if n := r.copied.Load() + r.deleted.Load() - before; n > 0 {
		r.log.Info("replication comparison copied or deleted keys", "rule", w.rule.ID, "bucket", w.rule.Bucket, "keys", n)
	}
}

func (r *Replicator) targetKeys(ctx context.Context, w *worker) iter.Seq2[s3client.Object, error] {
	return func(yield func(s3client.Object, error) bool) {
		errStop := errors.New("stop")
		err := w.client().ListObjects(ctx, w.rule.TargetBucket, w.rule.Prefix, func(o s3client.Object) error {
			if !yield(o, nil) {
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			yield(s3client.Object{}, err)
		}
	}
}
