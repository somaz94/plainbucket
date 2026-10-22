package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/storage"
)

// reset clears state a previous Execute left behind, such as --help or a cancelled context.
func reset(t *testing.T, args ...string) {
	t.Helper()
	_ = rootCmd.Flags().Set("help", "false")
	rootCmd.SetContext(context.Background())
	rootCmd.SetArgs(args)
}

func TestHelp(t *testing.T) {
	t.Cleanup(func() { reset(t) })
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"--help"})
	if err := Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "readable without the server") {
		t.Errorf("help = %q", buf.String())
	}
}

func TestInvalidConfig(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	reset(t)
	if err := Execute(); err == nil || !strings.Contains(err.Error(), "ACCESS_KEY") {
		t.Errorf("err = %v", err)
	}
}

func TestServeUntilCancelled(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "ak")
	t.Setenv("PLAINBUCKET_SECRET_KEY", "sk")
	t.Setenv("PLAINBUCKET_ROOT", t.TempDir())
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:0")
	reset(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rootCmd.ExecuteContext(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop when its context was cancelled")
	}
}

func TestVersion(t *testing.T) {
	reset(t, "version")
	if err := Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthURL(t *testing.T) {
	for addr, want := range map[string]string{
		":9000":          "http://127.0.0.1:9000/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"10.1.2.3:9100":  "http://10.1.2.3:9100/healthz",
		"localhost:9000": "http://localhost:9000/healthz",
	} {
		cfg := config.Default()
		cfg.Address = addr
		if got := healthURL(cfg); got != want {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}
	cfg := config.Default()
	cfg.TLSCert, cfg.TLSKey = "c", "k"
	if got := healthURL(cfg); got != "https://127.0.0.1:9000/healthz" {
		t.Errorf("tls: %s", got)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ok.Close()
	if err := healthcheck(ok.URL+"/healthz", time.Second); err != nil {
		t.Errorf("healthy: %v", err)
	}
	if err := healthcheck(ok.URL+"/other", time.Second); err == nil {
		t.Error("404 accepted")
	}

	// Through the command, against an address nothing listens on, with no access key set.
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:1")
	reset(t, "healthcheck")
	if err := Execute(); err == nil {
		t.Error("healthcheck passed with no server")
	}
	u, _ := url.Parse(strings.Replace(ok.URL, "https", "http", 1))
	t.Setenv("PLAINBUCKET_ADDRESS", u.Host)
	t.Setenv("PLAINBUCKET_TLS_CERT", "unused")
	t.Setenv("PLAINBUCKET_TLS_KEY", "unused")
	reset(t, "healthcheck")
	if err := Execute(); err != nil {
		t.Errorf("healthcheck over TLS: %v", err)
	}
}

func TestFsckCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	t.Setenv("PLAINBUCKET_ROOT", root)
	_ = os.MkdirAll(filepath.Join(root, "bkt"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "bkt", "dropped-in"), []byte("x"), 0o644)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	t.Cleanup(func() { rootCmd.SetOut(nil); fsckRepair = false })

	reset(t, "fsck")
	if err := Execute(); err == nil || !strings.Contains(out.String(), "bkt/dropped-in") {
		t.Errorf("dry run: %v\n%s", err, out.String())
	}
	out.Reset()
	reset(t, "fsck", "--repair")
	if err := Execute(); err != nil || !strings.Contains(out.String(), "(fixed)") {
		t.Errorf("repair: %v\n%s", err, out.String())
	}
	out.Reset()
	fsckRepair = false
	reset(t, "fsck")
	if err := Execute(); err != nil || !strings.Contains(out.String(), "0 problem(s)") {
		t.Errorf("after repair: %v\n%s", err, out.String())
	}

	store, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		t.Fatal(err)
	}
	reset(t, "fsck")
	if err := Execute(); err == nil || !strings.Contains(err.Error(), "stop the server") {
		t.Errorf("fsck under a running server: %v", err)
	}
}
