package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

// syncBuffer is a log sink the test can read while the server writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startReloading is start with a reload source; it returns the base URL and the signal
// channel, and stops the server when the test ends.
func startReloading(t *testing.T, configFile string, logs *syncBuffer) (string, chan os.Signal) {
	t.Helper()
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	src := reloadSource{configFile: configFile, hup: hup, every: 20 * time.Millisecond}
	go func() { done <- serve(ctx, cfg, NewLogger(logs, cfg), ln, src) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	scheme := "http"
	if cfg.HasTLS() {
		scheme = "https"
	}
	return scheme + "://" + ln.Addr().String(), hup
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 250 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func listAs(t *testing.T, url, ak, sk string) int {
	t.Helper()
	return doAs(t, http.MethodGet, url+"/", ak, sk, "")
}

func doAs(t *testing.T, method, url, ak, sk, body string) int {
	t.Helper()
	r, _ := http.NewRequest(method, url, strings.NewReader(body))
	sigv4test.Signer{AccessKey: ak, SecretKey: sk, Region: "us-east-1"}.Sign(r, "UNSIGNED-PAYLOAD", time.Now())
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadOnFileChangeAndSignal(t *testing.T) {
	dir := t.TempDir()
	creds, file := filepath.Join(dir, "creds.yaml"), filepath.Join(dir, "config.yaml")
	writeFile(t, creds, "[]\n")
	base := "root: " + filepath.Join(dir, "data") + "\naccess-key: ak\nsecret-key: sk\ncredentials-file: " + creds + "\n"
	writeFile(t, file, base)
	logs := &syncBuffer{}
	url, hup := startReloading(t, file, logs)

	eventually(t, "the server", func() bool { return listAs(t, url, "ak", "sk") == http.StatusOK })
	if code := listAs(t, url, "ak2", "sk2"); code != http.StatusForbidden {
		t.Fatalf("unknown key: %d", code)
	}
	// A new key in the credentials file is picked up without a signal.
	writeFile(t, creds, "- {access-key: ak2, secret-key: sk2}\n")
	eventually(t, "the new key", func() bool { return listAs(t, url, "ak2", "sk2") == http.StatusOK })
	if !strings.Contains(logs.String(), `msg="configuration reloaded" cause="file change"`) {
		t.Errorf("reload not logged:\n%s", logs.String())
	}

	// A broken file keeps the running configuration.
	writeFile(t, file, base+"bogus-setting: 1\n")
	eventually(t, "the failure", func() bool { return strings.Contains(logs.String(), "reload failed") })
	if code := listAs(t, url, "ak2", "sk2"); code != http.StatusOK {
		t.Errorf("key lost after a failed reload: %d", code)
	}

	// The first limit measures usage, which a refusing limit checks writes against.
	writeFile(t, file, base+"buckets: {lim: {max-size: 100}}\n")
	eventually(t, "the limit", func() bool { return strings.Contains(logs.String(), "buckets=1") })

	// More free space than any test machine has refuses writes once reloaded.
	if code := doAs(t, http.MethodPut, url+"/full", "ak", "sk", ""); code != http.StatusOK {
		t.Fatalf("create bucket: %d", code)
	}
	writeFile(t, file, base+"min-free-space: 4611686018427387904\n")
	eventually(t, "min-free-space", func() bool {
		return doAs(t, http.MethodPut, url+"/full/k", "ak", "sk", "x") == http.StatusInsufficientStorage
	})

	// The read-only mode turns on and off without a restart.
	writeFile(t, file, base+"read-only: true\n")
	eventually(t, "read-only on", func() bool {
		return doAs(t, http.MethodPut, url+"/full/k", "ak", "sk", "x") == http.StatusServiceUnavailable
	})
	if !strings.Contains(logs.String(), "read-only mode changed") {
		t.Error("read-only change not logged")
	}
	writeFile(t, file, base)
	eventually(t, "read-only off", func() bool { return doAs(t, http.MethodPut, url+"/full/k", "ak", "sk", "x") == http.StatusOK })

	// A setting that needs a restart is reported; a signal reloads without a file change.
	writeFile(t, file, base+"metrics: true\n")
	eventually(t, "the restart warning", func() bool { return strings.Contains(logs.String(), "settings=[metrics]") })
	hup <- syscall.SIGHUP
	eventually(t, "the signal", func() bool { return strings.Contains(logs.String(), `cause=signal`) })
}

// hookSink is a webhook endpoint that records each request's event and whether its signature verified.
type hookSink struct {
	mu   sync.Mutex
	sigs []string
}

func newHookSink(t *testing.T, secret func() []byte) (*hookSink, string) {
	sink := &hookSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ok := r.Header.Get(notify.HeaderSignature) == notify.Sign(secret(), r.Header.Get(notify.HeaderTimestamp), body)
		sink.mu.Lock()
		sink.sigs = append(sink.sigs, fmt.Sprintf("%s %v", r.Header.Get(notify.HeaderEvent), ok))
		sink.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return sink, srv.URL
}

func (s *hookSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sigs...)
}

func TestReloadSwapsWebhookSecret(t *testing.T) {
	dir := t.TempDir()
	secretFile, file := filepath.Join(dir, "hook"), filepath.Join(dir, "config.yaml")
	writeFile(t, secretFile, "one\n")
	var mu sync.Mutex
	key := []byte("one")
	sink, hook := newHookSink(t, func() []byte { mu.Lock(); defer mu.Unlock(); return key })
	base := "root: " + filepath.Join(dir, "data") + "\naccess-key: ak\nsecret-key: sk\n"
	target := func(secret string) string { return "{url: \"" + hook + "\", secret-file: " + secret + "}" }
	writeFile(t, file, base+"buckets: {events: {notify: ["+target(secretFile)+"]}}\n")
	logs := &syncBuffer{}
	url, _ := startReloading(t, file, logs)
	reloads := func() int { return strings.Count(logs.String(), "configuration reloaded") }

	eventually(t, "the server", func() bool { return doAs(t, http.MethodPut, url+"/events", "ak", "sk", "") == http.StatusOK })
	doAs(t, http.MethodPut, url+"/events/a", "ak", "sk", "x")
	eventually(t, "the first event", func() bool { return len(sink.got()) == 1 })

	// Rotating the secret file alone is a change the server reloads. A different length
	// keeps the change visible where mtimes are coarse.
	mu.Lock()
	key = []byte("two-2")
	mu.Unlock()
	writeFile(t, secretFile, "two-2\n")
	eventually(t, "the reload", func() bool { return reloads() == 1 })
	doAs(t, http.MethodDelete, url+"/events/a", "ak", "sk", "")
	eventually(t, "the second event", func() bool { return len(sink.got()) == 2 })
	if got := strings.Join(sink.got(), ","); got != "ObjectCreated:Put true,ObjectRemoved:Delete true" {
		t.Errorf("events %s", got)
	}

	// A secret-file that does not exist yet fails the reload, which is retried once it appears.
	later := filepath.Join(dir, "later")
	writeFile(t, file, base+"buckets: {events: {notify: ["+target(later)+"]}}\n")
	eventually(t, "the failure", func() bool { return strings.Contains(logs.String(), "reload failed") })
	mu.Lock()
	key = []byte("three")
	mu.Unlock()
	writeFile(t, later, "three\n")
	eventually(t, "the retried reload", func() bool { return reloads() == 2 })
	doAs(t, http.MethodPut, url+"/events/b", "ak", "sk", "x")
	eventually(t, "the third event", func() bool { return len(sink.got()) == 3 })
	if got := sink.got()[2]; got != "ObjectCreated:Put true" {
		t.Errorf("event after the retry %s", got)
	}
}

func TestLifecycleEvents(t *testing.T) {
	sink, hook := newHookSink(t, func() []byte { return nil })
	n := notify.New("us-east-1", slog.New(slog.DiscardHandler))
	defer n.Close(context.Background())
	n.SetTargets(map[string][]notify.Target{"b": {{URL: hook}}})
	lifecycleEvents(n)("b", "old")
	eventually(t, "the event", func() bool { return len(sink.got()) == 1 })
	if got := sink.got()[0]; got != "LifecycleExpiration:Delete false" {
		t.Errorf("event %s", got)
	}
}

func TestReloadSwapsTheCertificate(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t)
	file := filepath.Join(dir, "config.yaml")
	writeFile(t, file, "root: "+filepath.Join(dir, "data")+"\naccess-key: ak\nsecret-key: sk\ntls-cert: "+cert+"\ntls-key: "+key+"\n")
	logs := &syncBuffer{}
	url, _ := startReloading(t, file, logs)
	addr := strings.TrimPrefix(url, "https://")

	served := func() []byte {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test cert
		if err != nil {
			return nil
		}
		defer func() { _ = conn.Close() }()
		return conn.ConnectionState().PeerCertificates[0].Raw
	}
	var first []byte
	eventually(t, "the server", func() bool { first = served(); return first != nil })

	// A broken certificate keeps the one in force.
	good, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cert, "not a certificate")
	eventually(t, "the failed reload", func() bool { return strings.Contains(logs.String(), "reload failed") })
	if c := served(); !bytes.Equal(c, first) {
		t.Fatal("certificate changed after a failed reload")
	}
	writeFile(t, cert, string(good))

	newCert, newKey := writeCert(t)
	for _, p := range [][2]string{{newCert, cert}, {newKey, key}} {
		b, err := os.ReadFile(p[0])
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, p[1], string(b))
	}
	eventually(t, "the new certificate", func() bool { c := served(); return c != nil && !bytes.Equal(c, first) })
}

func TestRestartNeededIgnoresReloadableSettings(t *testing.T) {
	a := config.Default()
	b := config.Default()
	b.AccessKey, b.Buckets, b.MinFreeSpace, b.ReadOnly = "other", map[string]config.Bucket{"x": {Public: true}}, 1, true
	if got := a.RestartNeeded(b); len(got) != 0 {
		t.Errorf("reloadable settings reported: %v", got)
	}
	b.Address, b.TLSCert, b.TLSKey = ":1", "c", "k"
	if got := strings.Join(a.RestartNeeded(b), ","); got != "address,tls-cert" {
		t.Errorf("restart needed = %s", got)
	}
}
