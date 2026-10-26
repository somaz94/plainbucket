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
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
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
	if tr := bucketTrash(cfg); len(tr) != 1 || tr["docs"] != 48*time.Hour {
		t.Errorf("trash = %+v", tr)
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
