// Package notify posts bucket events to webhooks as S3 event notification JSON, signed
// with HMAC-SHA256. Delivery is best-effort: each target has a bounded in-memory queue,
// failed posts are retried with backoff, and what is queued at exit is lost.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/version"
)

// Event names, as S3 writes them in a record's eventName.
const (
	ObjectCreatedPut      = "ObjectCreated:Put"
	ObjectCreatedPost     = "ObjectCreated:Post"
	ObjectCreatedCopy     = "ObjectCreated:Copy"
	ObjectCreatedComplete = "ObjectCreated:CompleteMultipartUpload"
	ObjectRemovedDelete   = "ObjectRemoved:Delete"
	LifecycleExpiration   = "LifecycleExpiration:Delete"
)

var eventNames = []string{
	ObjectCreatedPut, ObjectCreatedPost, ObjectCreatedCopy, ObjectCreatedComplete,
	ObjectRemovedDelete, LifecycleExpiration,
}

// Signature headers: Signature is "sha256=" + hex HMAC-SHA256 of Timestamp + "." + body.
const (
	HeaderTimestamp = "X-Plainbucket-Timestamp"
	HeaderSignature = "X-Plainbucket-Signature"
	HeaderEvent     = "X-Plainbucket-Event"
)

const (
	queueSize      = 1000
	attemptTimeout = 10 * time.Second
	ownerID        = "plainbucket" // matches the owner the S3 API reports
)

var defaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// ValidEvent reports whether p names an event or, with a trailing "*", a group of them,
// in S3's form ("s3:ObjectCreated:*", "s3:ObjectRemoved:Delete").
func ValidEvent(p string) bool {
	if !strings.HasPrefix(p, "s3:") {
		return false
	}
	return slices.ContainsFunc(eventNames, func(name string) bool { return matchEvent(p, name) })
}

func matchEvent(pattern, name string) bool {
	p := strings.TrimPrefix(pattern, "s3:")
	if group, ok := strings.CutSuffix(p, "*"); ok {
		return strings.HasSuffix(group, ":") && strings.HasPrefix(name, group)
	}
	return p == name
}

// Target is one webhook.
type Target struct {
	ID     string // the record's configurationId
	URL    string
	Events []string // ValidEvent patterns; empty = all
	Prefix string
	Suffix string
	Secret []byte // HMAC key; empty sends the events unsigned
}

func (t Target) wants(e Event) bool {
	if !strings.HasPrefix(e.Key, t.Prefix) || !strings.HasSuffix(e.Key, t.Suffix) {
		return false
	}
	return len(t.Events) == 0 || slices.ContainsFunc(t.Events, func(p string) bool { return matchEvent(p, e.Name) })
}

// Event is one change to an object.
type Event struct {
	Name        string
	Bucket, Key string
	Size        int64 // reported for ObjectCreated events
	ETag        string
	Principal   string // the access key, or "" for the server itself
	SourceIP    string
	RequestID   string
	Time        time.Time
}

// Notifier routes events to the targets of their bucket.
type Notifier struct {
	region  string
	log     *slog.Logger
	client  *http.Client
	backoff []time.Duration
	now     func() time.Time

	ctx    context.Context // canceled by Close; early, mid-delivery, when it gives up waiting
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Publish holds mu for reading, so a worker is stopped only after every event sent to
	// it is in its queue, where the stopped worker still finds it.
	mu      sync.RWMutex
	workers map[string]*worker
	closed  bool
	routes  map[string][]*worker // by bucket

	lastSeq               atomic.Int64
	sent, failed, dropped atomic.Int64
}

type worker struct {
	t     Target
	queue chan delivery
	stop  chan struct{}
}

type delivery struct {
	name string
	body []byte
}

// New returns a Notifier without targets.
func New(region string, log *slog.Logger) *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{
		region: region, log: log, backoff: defaultBackoff, now: time.Now, ctx: ctx, cancel: cancel,
		// A redirect would turn the POST into a GET; it counts as a failure instead.
		client:  &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		workers: map[string]*worker{},
	}
	return n
}

// SetTargets replaces the targets, by bucket; it is safe while events are published.
// A target that stays keeps its queue; one that goes delivers what it holds, then stops.
func (n *Notifier) SetTargets(targets map[string][]Target) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	next := map[string]*worker{}
	routes := map[string][]*worker{}
	for bucket, ts := range targets {
		for _, t := range ts {
			k := targetKey(t)
			w := next[k]
			if w == nil {
				if w = n.workers[k]; w == nil {
					w = n.start(t)
				}
				next[k] = w
			}
			routes[bucket] = append(routes[bucket], w)
		}
	}
	for k, w := range n.workers {
		if next[k] == nil {
			close(w.stop)
		}
	}
	n.workers = next
	n.routes = routes
}

func targetKey(t Target) string {
	raw, _ := json.Marshal(t)
	return string(raw)
}

func (n *Notifier) start(t Target) *worker {
	w := &worker{t: t, queue: make(chan delivery, queueSize), stop: make(chan struct{})}
	n.wg.Go(func() { n.run(w) })
	return w
}

func (n *Notifier) run(w *worker) {
	for {
		select {
		case d := <-w.queue:
			n.deliver(w.t, d)
		case <-w.stop:
			for {
				select {
				case d := <-w.queue:
					n.deliver(w.t, d)
				default:
					return
				}
			}
		}
	}
}

// Publish queues e for every target of its bucket that wants it. It never blocks: a
// full queue drops the event.
func (n *Notifier) Publish(e Event) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	ws := n.routes[e.Bucket]
	if len(ws) == 0 {
		return
	}
	seq := n.sequencer(e.Time)
	for _, w := range ws {
		if !w.t.wants(e) {
			continue
		}
		body, err := json.Marshal(message{Records: []record{n.record(e, w.t.ID, seq)}})
		if err != nil {
			continue
		}
		select {
		case w.queue <- delivery{name: e.Name, body: body}:
		default:
			n.dropped.Add(1)
			n.log.Warn("event notification dropped: queue full", "target", w.t.ID, "url", redactURL(w.t.URL), "event", e.Name, "bucket", e.Bucket, "key", e.Key)
		}
	}
}

// sequencer orders the events of a key as S3's does: a later event has a greater value,
// compared as strings. Unix nanoseconds keep that across restarts.
func (n *Notifier) sequencer(t time.Time) string {
	for {
		last := n.lastSeq.Load()
		v := max(t.UnixNano(), last+1)
		if n.lastSeq.CompareAndSwap(last, v) {
			return fmt.Sprintf("%016X", v)
		}
	}
}

// permanent is a failure that retrying will not change: a response status, or err.
type permanent struct {
	status int
	err    error
}

func (p permanent) Error() string {
	if p.err != nil {
		return p.err.Error()
	}
	return "status " + strconv.Itoa(p.status)
}

func (n *Notifier) deliver(t Target, d delivery) {
	for attempt := 0; ; attempt++ {
		if n.ctx.Err() != nil {
			n.dropped.Add(1) // Close gave up; it reports the count once
			return
		}
		err := n.post(t, d)
		if err == nil {
			n.sent.Add(1)
			return
		}
		if n.ctx.Err() != nil {
			n.dropped.Add(1)
			return
		}
		var p permanent
		if errors.As(err, &p) || attempt >= len(n.backoff) {
			n.failed.Add(1)
			n.log.Warn("event notification failed", "target", t.ID, "url", redactURL(t.URL), "event", d.name, "attempts", attempt+1, "err", err)
			return
		}
		select {
		case <-time.After(n.backoff[attempt]):
		case <-n.ctx.Done():
		}
	}
}

func (n *Notifier) post(t Target, d delivery) error {
	ctx, cancel := context.WithTimeout(n.ctx, attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(d.body))
	if err != nil {
		return permanent{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plainbucket/"+version.Version)
	req.Header.Set(HeaderEvent, d.name)
	if len(t.Secret) > 0 {
		// Taken per attempt, so a receiver may refuse old timestamps without refusing retries.
		ts := strconv.FormatInt(n.now().Unix(), 10)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, Sign(t.Secret, ts, d.body))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// A *url.Error repeats the URL, query included, which may hold a token.
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return permanent{status: resp.StatusCode}
}

// Sign returns the HeaderSignature value for a body sent at timestamp ts.
func Sign(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// redactURL drops what may carry a credential: user info and the query.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// Stats returns the deliveries made, those given up after retries or refused, those
// dropped on a full queue, and the events still queued.
func (n *Notifier) Stats() (sent, failed, dropped int64, queued int) {
	n.mu.RLock()
	for _, w := range n.workers {
		queued += len(w.queue)
	}
	n.mu.RUnlock()
	return n.sent.Load(), n.failed.Load(), n.dropped.Load(), queued
}

// Close stops taking events and delivers those queued until ctx is done; then it cancels
// the deliveries in flight and drops the rest.
func (n *Notifier) Close(ctx context.Context) {
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		for _, w := range n.workers {
			close(w.stop)
		}
		n.workers = map[string]*worker{}
		n.routes = nil
	}
	n.mu.Unlock()
	before := n.dropped.Load()
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		n.cancel()
		<-done
		if lost := n.dropped.Load() - before; lost > 0 {
			n.log.Warn("event notifications dropped at shutdown", "count", lost)
		}
	}
	n.cancel()
}

type message struct {
	Records []record `json:"Records"`
}

type record struct {
	EventVersion      string            `json:"eventVersion"`
	EventSource       string            `json:"eventSource"`
	AWSRegion         string            `json:"awsRegion"`
	EventTime         string            `json:"eventTime"`
	EventName         string            `json:"eventName"`
	UserIdentity      principal         `json:"userIdentity"`
	RequestParameters requestParameters `json:"requestParameters"`
	ResponseElements  map[string]string `json:"responseElements"`
	S3                entity            `json:"s3"`
}

type principal struct {
	PrincipalID string `json:"principalId"`
}

type requestParameters struct {
	SourceIPAddress string `json:"sourceIPAddress"`
}

type entity struct {
	SchemaVersion   string       `json:"s3SchemaVersion"`
	ConfigurationID string       `json:"configurationId"`
	Bucket          bucketEntity `json:"bucket"`
	Object          objectEntity `json:"object"`
}

type bucketEntity struct {
	Name          string    `json:"name"`
	OwnerIdentity principal `json:"ownerIdentity"`
	ARN           string    `json:"arn"`
}

type objectEntity struct {
	Key       string `json:"key"`
	Size      *int64 `json:"size,omitempty"`
	ETag      string `json:"eTag,omitempty"`
	Sequencer string `json:"sequencer"`
}

func (n *Notifier) record(e Event, configID, seq string) record {
	// Form-encoded as S3 does it, which leaves the slashes.
	obj := objectEntity{Key: strings.ReplaceAll(url.QueryEscape(e.Key), "%2F", "/"), Sequencer: seq}
	if strings.HasPrefix(e.Name, "ObjectCreated:") {
		size := e.Size
		obj.Size, obj.ETag = &size, strings.Trim(e.ETag, `"`)
	}
	who := e.Principal
	if who == "" {
		who = ownerID
	}
	return record{
		EventVersion: "2.1", EventSource: "aws:s3", AWSRegion: n.region,
		EventTime:         e.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
		EventName:         e.Name,
		UserIdentity:      principal{who},
		RequestParameters: requestParameters{e.SourceIP},
		ResponseElements:  map[string]string{"x-amz-request-id": e.RequestID, "x-amz-id-2": e.RequestID},
		S3: entity{
			SchemaVersion: "1.0", ConfigurationID: configID,
			Bucket: bucketEntity{Name: e.Bucket, OwnerIdentity: principal{ownerID}, ARN: "arn:aws:s3:::" + e.Bucket},
			Object: obj,
		},
	}
}
