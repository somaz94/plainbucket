package config

import (
	"crypto/tls"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "ak")
	t.Setenv("PLAINBUCKET_SECRET_KEY", "sk")
}

func TestDefaultsAndEnv(t *testing.T) {
	setRequired(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "/data" || cfg.Address != ":9000" || cfg.Region != "us-east-1" || cfg.UploadExpiry != 7*24*time.Hour ||
		cfg.BodyTimeout != time.Minute || cfg.Metrics || cfg.HasTLS() {
		t.Errorf("defaults = %+v", cfg)
	}

	t.Setenv("PLAINBUCKET_ROOT", "/srv")
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:9100")
	t.Setenv("PLAINBUCKET_REGION", "eu-west-1")
	t.Setenv("PLAINBUCKET_MAX_OBJECT_SIZE", "1024")
	t.Setenv("PLAINBUCKET_UPLOAD_EXPIRY", "2h")
	t.Setenv("PLAINBUCKET_BODY_TIMEOUT", "0s")
	t.Setenv("PLAINBUCKET_METRICS", "true")
	t.Setenv("PLAINBUCKET_LOG_FORMAT", "JSON")
	t.Setenv("PLAINBUCKET_DEBUG", "yes")
	t.Setenv("PLAINBUCKET_TLS_CERT", "c.pem")
	t.Setenv("PLAINBUCKET_TLS_KEY", "k.pem")
	t.Setenv("PLAINBUCKET_TLS_MIN_VERS", "tls13")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "/srv" || cfg.Address != "127.0.0.1:9100" || cfg.Region != "eu-west-1" || cfg.MaxObjectSize != 1024 ||
		cfg.UploadExpiry != 2*time.Hour || cfg.BodyTimeout != 0 || !cfg.Metrics || cfg.LogFormat != "json" || !cfg.Debug || !cfg.HasTLS() || cfg.TLSMinVersion() != tls.VersionTLS13 {
		t.Errorf("env = %+v", cfg)
	}
}

func TestYAMLThenEnv(t *testing.T) {
	setRequired(t)
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("root: /yaml\nregion: ap-northeast-2\nupload-expiry: 1h\n"), 0o600)
	t.Setenv("PLAINBUCKET_REGION", "us-west-2")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "/yaml" || cfg.Region != "us-west-2" || cfg.UploadExpiry != time.Hour {
		t.Errorf("cfg = %+v", cfg)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := Load(empty); err != nil {
		t.Errorf("empty file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("roots: /typo\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"no keys":       {"PLAINBUCKET_ACCESS_KEY": ""},
		"no root":       {"PLAINBUCKET_ROOT": ""},
		"no address":    {"PLAINBUCKET_ADDRESS": ""},
		"no region":     {"PLAINBUCKET_REGION": ""},
		"negative size": {"PLAINBUCKET_MAX_OBJECT_SIZE": "-1"},
		"zero expiry":   {"PLAINBUCKET_UPLOAD_EXPIRY": "0s"},
		"negative body": {"PLAINBUCKET_BODY_TIMEOUT": "-1s"},
		"half tls":      {"PLAINBUCKET_TLS_CERT": "c.pem"},
		"tls version":   {"PLAINBUCKET_TLS_MIN_VERS": "TLS10"},
		"log format":    {"PLAINBUCKET_LOG_FORMAT": "xml"},
		"bad bool":      {"PLAINBUCKET_DEBUG": "maybe"},
		"bad duration":  {"PLAINBUCKET_UPLOAD_EXPIRY": "7d"},
		"bad size":      {"PLAINBUCKET_MAX_OBJECT_SIZE": "10Gi"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(""); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestEmptyTypedValueIsUnset(t *testing.T) {
	setRequired(t)
	t.Setenv("PLAINBUCKET_DEBUG", " ")
	t.Setenv("PLAINBUCKET_MAX_OBJECT_SIZE", "")
	t.Setenv("PLAINBUCKET_UPLOAD_EXPIRY", "")
	t.Setenv("PLAINBUCKET_DEBUG", "false")
	if cfg, err := Load(""); err != nil || cfg.Debug || cfg.MaxObjectSize != 0 {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}
}

func TestLogValueHidesSecrets(t *testing.T) {
	cfg := Default()
	cfg.AccessKey, cfg.SecretKey = "AKVISIBLE", "SECRETVALUE"
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "config", cfg)
	if strings.Contains(b.String(), "SECRETVALUE") || strings.Contains(b.String(), "AKVISIBLE") || !strings.Contains(b.String(), "secret_key=true") {
		t.Errorf("log = %s", b.String())
	}
	if cfg.TLSMinVersion() != tls.VersionTLS12 {
		t.Error("default TLS version")
	}
}
