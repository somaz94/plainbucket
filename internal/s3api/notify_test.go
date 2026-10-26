package s3api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/notify"
)

type gotEvent struct {
	name, key, principal, ip, requestID string
	size                                *int64
}

// eventSink is a webhook endpoint that keeps the events it is sent, in order.
type eventSink struct {
	mu     sync.Mutex
	events []gotEvent
}

func newEventSink(t *testing.T) (*eventSink, *httptest.Server) {
	sink := &eventSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Records []struct {
				EventName    string
				UserIdentity struct{ PrincipalID string }
				Request      struct {
					SourceIPAddress string
				} `json:"requestParameters"`
				Response map[string]string `json:"responseElements"`
				S3       struct {
					Object struct {
						Key  string
						Size *int64
					}
				}
			}
		}
		if err := json.Unmarshal(body, &msg); err != nil || len(msg.Records) != 1 {
			t.Errorf("event body %s: %v", body, err)
			return
		}
		rec := msg.Records[0]
		sink.mu.Lock()
		sink.events = append(sink.events, gotEvent{rec.EventName, rec.S3.Object.Key, rec.UserIdentity.PrincipalID,
			rec.Request.SourceIPAddress, rec.Response["x-amz-request-id"], rec.S3.Object.Size})
		sink.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return sink, srv
}

// wait returns the events once n have arrived.
func (s *eventSink) wait(t *testing.T, n int) []gotEvent {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		got := append([]gotEvent(nil), s.events...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("timed out waiting for %d events", n)
	return nil
}

func TestEventNotifications(t *testing.T) {
	sink, srv := newEventSink(t)
	n := notify.New("us-east-1", slog.New(slog.DiscardHandler))
	t.Cleanup(func() { n.Close(context.Background()) })
	n.SetTargets(map[string][]notify.Target{"bkt": {{ID: "t", URL: srv.URL}}})
	x := newHarness(t, func(o *Options) {
		o.Notifier = n
		o.Console = true
		o.Metrics = true
	})
	x.h.store.SetTrash(map[string]time.Duration{"bkt": time.Hour})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/other", nil, nil, http.StatusOK)

	put := x.mustDo(http.MethodPut, "/bkt/a%20b", []byte("hello"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/other/k", []byte("x"), nil, http.StatusOK)
	x.mustDo(http.MethodPut, "/bkt/c", nil, map[string]string{"X-Amz-Copy-Source": "/bkt/a%20b"}, http.StatusOK)
	x.multipartObject("m", "hello", "x")
	exp := testNow.Add(time.Hour)
	conds := []any{map[string]string{"bucket": "bkt"}, []string{"starts-with", "$key", ""}}
	if rec := x.post("/bkt", postForm{fields: signedForm(x.signer, exp, conds, [2]string{"key", "p"}), filename: "f", file: "post"}); rec.Code != http.StatusNoContent {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	x.mustDo(http.MethodDelete, "/bkt/missing", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodDelete, "/bkt/a%20b", nil, nil, http.StatusNoContent)
	x.mustDo(http.MethodPost, "/bkt?delete", []byte(`<Delete><Object><Key>c</Key></Object><Object><Key>nope</Key></Object></Delete>`), nil, http.StatusOK)

	var trash consoleTrashResponse
	_ = json.Unmarshal(x.mustDo(http.MethodGet, consoleTrashPath+"?bucket=bkt", nil, nil, http.StatusOK).Body.Bytes(), &trash)
	var id string
	for _, e := range trash.Entries {
		if e.Key == "c" {
			id = e.ID
		}
	}
	x.mustDo(http.MethodPost, consoleTrashPath+"?bucket=bkt&action=restore&id="+id, nil, nil, http.StatusOK)

	got := sink.wait(t, 7)
	var names []string
	for _, e := range got {
		names = append(names, e.name+" "+e.key)
	}
	want := []string{
		"ObjectCreated:Put a+b", "ObjectCreated:Copy c", "ObjectCreated:CompleteMultipartUpload m",
		"ObjectCreated:Post p", "ObjectRemoved:Delete a+b", "ObjectRemoved:Delete c", "ObjectCreated:Put c",
	}
	if strings.Join(names, ", ") != strings.Join(want, ", ") {
		t.Errorf("events:\n got %v\nwant %v", names, want)
	}
	first := got[0]
	if first.principal != "AK" || first.ip != "192.0.2.1" || first.requestID != put.Header().Get("X-Amz-Request-Id") ||
		first.size == nil || *first.size != 5 {
		t.Errorf("put event: %+v", first)
	}
	if got[2].size == nil || *got[2].size != 6 || got[4].size != nil {
		t.Errorf("sizes: multipart %v delete %v", got[2].size, got[4].size)
	}

	// The receiver answers before the notifier counts the delivery.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if sent, _, _, _ := n.Stats(); sent == 7 {
			break
		}
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{`plainbucket_notifications_total{result="sent"} 7`, "plainbucket_notification_queue 0"} {
		if !strings.Contains(rec.Body.String(), line) {
			t.Errorf("metrics lack %q", line)
		}
	}
}
