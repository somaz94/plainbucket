package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/ratelimit"
	"github.com/somaz94/plainbucket/internal/storage"
)

func testConfig(t *testing.T) *config.Config {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.AccessKey, cfg.SecretKey = "ak", "sk"
	return cfg
}

// start runs serve on a free port and returns its base URL and a stop function.
func start(t *testing.T, cfg *config.Config, logs io.Writer) (string, func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, cfg, NewLogger(logs, cfg), ln, reloadSource{}) }()
	scheme := "http"
	if cfg.HasTLS() {
		scheme = "https"
	}
	stop := func() error {
		cancel()
		select {
		case err := <-errCh:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("server did not stop")
			return nil
		}
	}
	return scheme + "://" + ln.Addr().String(), stop
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	for range 50 {
		resp, err := client.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered", url)
	return 0, ""
}

func TestServeHTTP(t *testing.T) {
	cfg := testConfig(t)
	cfg.Debug = true
	var logs bytes.Buffer
	url, stop := start(t, cfg, &logs)
	if code, body := get(t, http.DefaultClient, url+"/healthz"); code != http.StatusOK || body != "ok" {
		t.Errorf("healthz: %d %q", code, body)
	}
	if code, _ := get(t, http.DefaultClient, url+"/bucket"); code != http.StatusForbidden {
		t.Errorf("anonymous: %d", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"serving", "configuration", "server stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, ".plainbucket", "meta")); err != nil {
		t.Error("system directory not created")
	}
}

func TestServeTLS(t *testing.T) {
	cfg := testConfig(t)
	cfg.TLSCert, cfg.TLSKey = writeCert(t)
	url, stop := start(t, cfg, io.Discard)
	defer func() { _ = stop() }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	if code, _ := get(t, client, url+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz over TLS: %d", code)
	}
}

func TestServeErrors(t *testing.T) {
	cfg := testConfig(t)
	file := filepath.Join(cfg.Root, "not-a-dir")
	_ = os.WriteFile(file, nil, 0o600)
	cfg.Root = file
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	if err := serve(context.Background(), cfg, NewLogger(io.Discard, cfg), ln, reloadSource{}); err == nil {
		t.Error("root that is a file accepted")
	}

	cfg = testConfig(t)
	cfg.TLSCert, cfg.TLSKey = "/missing/cert.pem", "/missing/key.pem"
	ln, _ = net.Listen("tcp", "127.0.0.1:0")
	if err := serve(context.Background(), cfg, NewLogger(io.Discard, cfg), ln, reloadSource{}); err == nil {
		t.Error("missing TLS files accepted")
	}

	cfg = testConfig(t)
	cfg.Address = "256.0.0.1:bad"
	if err := Run(context.Background(), cfg, ""); err == nil {
		t.Error("bad address accepted")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	cfg := testConfig(t)
	cfg.Address = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestNewLogger(t *testing.T) {
	var b bytes.Buffer
	cfg := config.Default()
	cfg.LogFormat = "json"
	NewLogger(&b, cfg).Info("hello")
	if !strings.HasPrefix(b.String(), "{") {
		t.Errorf("json log = %q", b.String())
	}
	b.Reset()
	cfg.LogFormat = "text"
	NewLogger(&b, cfg).Debug("hidden")
	if b.Len() != 0 {
		t.Error("debug logged without DEBUG")
	}
}

func writeCert(t *testing.T) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	cert, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return cert, keyPath
}

func TestBucketPolicies(t *testing.T) {
	cfg := testConfig(t)
	cfg.Buckets = map[string]config.Bucket{
		"www": {Public: true, Website: &config.Website{Index: "index.html", Error: "404.html"},
			CORS: &config.CORS{AllowedOrigins: []string{"*"}, MaxAge: time.Minute}},
		"plain": {},
	}
	cfg.Buckets["cache"] = config.Bucket{ExpireAfter: time.Hour, MaxSize: 10, Evict: true}
	cfg.Buckets["docs"] = config.Bucket{TrashAfter: 48 * time.Hour}
	if l := bucketLimits(cfg); len(l) != 1 || l["cache"].MaxSize != 10 || !l["cache"].Evict {
		t.Errorf("limits = %+v", l)
	}
	cfg.Buckets["logs"] = config.Bucket{Lifecycle: []config.Lifecycle{{Prefix: "a/", DeleteAfter: time.Hour}}}
	if l := bucketLimits(cfg)["logs"]; len(l.Rules) != 1 || l.Rules[0].Prefix != "a/" || l.Rules[0].DeleteAfter != time.Hour {
		t.Errorf("lifecycle limit = %+v", l)
	}
	delete(cfg.Buckets, "logs")
	if tr := bucketTrash(cfg); len(tr) != 1 || tr["docs"] != 48*time.Hour {
		t.Errorf("trash = %+v", tr)
	}
	cfg.Buckets["site"] = config.Bucket{Public: true, Headers: []config.HeaderRule{{Suffix: ".html", Set: map[string]string{"Cache-Control": "no-cache"}}}}
	if h := bucketPolicies(cfg)["site"].Headers; len(h) != 1 || h[0].Suffix != ".html" || h[0].Set["Cache-Control"] != "no-cache" {
		t.Errorf("header rules = %+v", h)
	}
	cfg.Buckets["events"] = config.Bucket{Notify: []config.Notify{{ID: "a", URL: "http://h", Events: []string{"s3:ObjectCreated:*"}, Prefix: "p/", Suffix: ".x", Secret: []byte("k")}}}
	if nt := notifyTargets(cfg); len(nt) != 1 || len(nt["events"]) != 1 || nt["events"][0].ID != "a" || nt["events"][0].Suffix != ".x" ||
		string(nt["events"][0].Secret) != "k" || nt["events"][0].Events[0] != "s3:ObjectCreated:*" {
		t.Errorf("notify targets = %+v", nt)
	}
	got := bucketPolicies(cfg)
	www := got["www"]
	if !www.Public || www.Website.Error != "404.html" || www.CORS.MaxAge != time.Minute || got["plain"].Website != nil {
		t.Errorf("policies = %+v", got)
	}
}

func TestSecondServerOnTheSameRootFails(t *testing.T) {
	cfg := testConfig(t)
	url, stop := start(t, cfg, io.Discard)
	get(t, http.DefaultClient, url+"/healthz")
	defer func() { _ = stop() }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = serve(context.Background(), cfg, NewLogger(io.Discard, cfg), ln, reloadSource{})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("second server: %v", err)
	}
}

func TestRunScrub(t *testing.T) {
	root := t.TempDir()
	store, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObject("bkt", "k", strings.NewReader("data"), 4, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Damage that keeps size and mtime, which only reading the data shows.
	p := filepath.Join(root, "bkt", "k")
	fi, _ := os.Stat(p)
	_ = os.WriteFile(p, []byte("dat#"), 0o644)
	_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())

	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runScrub(ctx, store, 10*time.Millisecond, 0, slog.New(slog.NewTextHandler(logs, nil)))
		close(done)
	}()
	eventually(t, "a scrub", func() bool { return strings.Contains(logs.String(), "scrub finished") })
	cancel()
	<-done
	if !strings.Contains(logs.String(), `msg="object data is damaged" bucket=bkt key=k digest=etag`) ||
		!strings.Contains(logs.String(), "corrupt=1") {
		t.Errorf("logs:\n%s", logs.String())
	}
}

func TestRunScrubWaitsFromTheLastRun(t *testing.T) {
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Scrub(context.Background(), 0, func(storage.Corruption) {}); err != nil {
		t.Fatal(err)
	}
	// A run that just finished, as a restarted server finds it, puts the next one an interval away.
	logs := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	runScrub(ctx, store, time.Hour, 0, slog.New(slog.NewTextHandler(logs, nil)))
	if strings.Contains(logs.String(), "scrub finished") {
		t.Errorf("scrubbed again right away:\n%s", logs.String())
	}
}

func TestCredentialRateLimits(t *testing.T) {
	cfg := testConfig(t)
	cfg.RateLimit = &config.RateLimit{Requests: 10}
	cfg.Credentials = []config.Credential{
		{AccessKey: "own", SecretKey: "s", RateLimit: &config.RateLimit{Bytes: 1 << 20}},
		{AccessKey: "default", SecretKey: "s"},
	}
	got := map[string]ratelimit.Limit{}
	for _, c := range credentials(cfg) {
		got[c.AccessKey] = c.RateLimit
	}
	// A key's own limit replaces the default whole; the others take the default.
	if got["own"] != (ratelimit.Limit{Bytes: 1 << 20}) || got["default"] != (ratelimit.Limit{Requests: 10}) {
		t.Errorf("limits = %+v", got)
	}
	cfg.RateLimit = nil
	for _, c := range credentials(cfg) {
		if c.AccessKey == "default" && !c.RateLimit.Zero() {
			t.Errorf("no default, yet %+v", c.RateLimit)
		}
	}
}

func TestSyncErrorLogIsThrottled(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	hourly, always := syncErrorLog(logger, time.Hour), syncErrorLog(logger, 0)
	for range 3 {
		hourly("bkt", syscall.EIO)
	}
	if n := strings.Count(logs.String(), "could not be flushed"); n != 1 {
		t.Errorf("%d logs within the interval, want 1:\n%s", n, logs)
	}
	always("bkt", syscall.EIO)
	always("bkt", syscall.EIO)
	if n := strings.Count(logs.String(), "could not be flushed"); n != 3 {
		t.Errorf("%d logs, want 3", n)
	}
	hourly("bkt", fmt.Errorf("%w: %w", storage.ErrSyncUnsupported, syscall.EINVAL))
	if !strings.Contains(logs.String(), "level=WARN msg=\"acknowledged writes may not survive a power loss\"") {
		t.Errorf("no warning for an unsupported flush:\n%s", logs)
	}
}

// A filesystem that refuses directory flushes is reported when the server starts, before
// any write.
func TestServeChecksFlushAtStart(t *testing.T) {
	t.Cleanup(storage.SetSyncDirForTest(func(string) error { return syscall.EINVAL }))
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	writeFile(t, file, "root: "+filepath.Join(dir, "data")+"\naccess-key: ak\nsecret-key: sk\n")
	logs := &syncBuffer{}
	startReloading(t, file, logs)
	eventually(t, "the warning", func() bool {
		return strings.Contains(logs.String(), "acknowledged writes may not survive a power loss")
	})
}
