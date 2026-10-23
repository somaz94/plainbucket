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
		cfg.BodyTimeout != time.Minute || cfg.Metrics || cfg.Console || cfg.HasTLS() {
		t.Errorf("defaults = %+v", cfg)
	}

	t.Setenv("PLAINBUCKET_ROOT", "/srv")
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:9100")
	t.Setenv("PLAINBUCKET_REGION", "eu-west-1")
	t.Setenv("PLAINBUCKET_MAX_OBJECT_SIZE", "1024")
	t.Setenv("PLAINBUCKET_UPLOAD_EXPIRY", "2h")
	t.Setenv("PLAINBUCKET_BODY_TIMEOUT", "0s")
	t.Setenv("PLAINBUCKET_METRICS", "true")
	t.Setenv("PLAINBUCKET_CONSOLE", "1")
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
		cfg.UploadExpiry != 2*time.Hour || cfg.BodyTimeout != 0 || !cfg.Metrics || !cfg.Console || cfg.LogFormat != "json" || !cfg.Debug || !cfg.HasTLS() || cfg.TLSMinVersion() != tls.VersionTLS13 {
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
		"half key":      {"PLAINBUCKET_SECRET_KEY": ""},
		"public bucket": {"PLAINBUCKET_PUBLIC_BUCKETS": "ok-bucket,Bad_Name"},
		"creds missing": {"PLAINBUCKET_CREDENTIALS_FILE": "/nonexistent/creds.yaml"},
		"domain port":   {"PLAINBUCKET_DOMAINS": "s3.example.com:9000"},
		"domain scheme": {"PLAINBUCKET_DOMAINS": "https://s3.example.com"},
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

func TestCredentialsAndPublicBuckets(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds.yaml")
	_ = os.WriteFile(creds, []byte("- access-key: runner\n  secret-key: runner-secret\n  buckets: [runner-cache]\n- access-key: reader\n  secret-key: reader-secret\n  read-only: true\n"), 0o600)
	cfgFile := filepath.Join(dir, "c.yaml")
	_ = os.WriteFile(cfgFile, []byte("credentials:\n  - access-key: yaml\n    secret-key: yaml-secret\n"), 0o600)
	t.Setenv("PLAINBUCKET_CREDENTIALS_FILE", creds)
	t.Setenv("PLAINBUCKET_PUBLIC_BUCKETS", " site, ,assets ")
	t.Setenv("PLAINBUCKET_DOMAINS", "S3.Example.com,localhost")
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	all := cfg.AllCredentials()
	var keys []string
	for _, c := range all {
		keys = append(keys, c.AccessKey)
	}
	if strings.Join(keys, ",") != "ak,yaml,runner,reader" || all[2].Buckets[0] != "runner-cache" || !all[3].ReadOnly || all[0].ReadOnly {
		t.Errorf("credentials = %+v", all)
	}
	if strings.Join(cfg.Domains, ",") != "s3.example.com,localhost" {
		t.Errorf("domains = %q", cfg.Domains)
	}
	if strings.Join(cfg.PublicBuckets, ",") != "site,assets" {
		t.Errorf("public buckets = %q", cfg.PublicBuckets)
	}

	// Keys from the file alone are enough.
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	t.Setenv("PLAINBUCKET_SECRET_KEY", "")
	if _, err := Load(""); err != nil {
		t.Errorf("file-only keys: %v", err)
	}

	for name, body := range map[string]string{
		"duplicate":  "- {access-key: runner, secret-key: a}\n- {access-key: runner, secret-key: b}\n",
		"no secret":  "- {access-key: runner}\n",
		"bad bucket": "- {access-key: runner, secret-key: a, buckets: [UPPER]}\n",
		"unknown":    "- {access-key: runner, secret-key: a, bucket: [x]}\n",
	} {
		_ = os.WriteFile(creds, []byte(body), 0o600)
		if _, err := Load(""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = os.WriteFile(creds, nil, 0o600)
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "no access key") {
		t.Errorf("no keys at all: %v", err)
	}
}

func TestBuckets(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	_ = os.WriteFile(path, []byte(`buckets:
  www:
    public: true
    website: {error: 404.html}
    cors:
      allowed-origins: ["https://*.example.com"]
      allowed-methods: [get, put]
      max-age: 1h
  runner-cache:
    expire-after: 336h
    max-size: 1073741824
    evict: true
`), 0o600)
	t.Setenv("PLAINBUCKET_PUBLIC_BUCKETS", "assets")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	www := cfg.Buckets["www"]
	if !www.Public || www.Website.Index != "index.html" || www.Website.Error != "404.html" ||
		strings.Join(www.CORS.AllowedMethods, ",") != "GET,PUT" || www.CORS.MaxAge != time.Hour {
		t.Errorf("www = %+v %+v %+v", www, www.Website, www.CORS)
	}
	if rc := cfg.Buckets["runner-cache"]; rc.ExpireAfter != 336*time.Hour || rc.MaxSize != 1<<30 || !rc.Evict {
		t.Errorf("runner-cache = %+v", rc)
	}
	if !cfg.Buckets["assets"].Public {
		t.Error("PLAINBUCKET_PUBLIC_BUCKETS not merged")
	}

	for name, body := range map[string]string{
		"bad name":        "buckets: {Bad_Name: {public: true}}\n",
		"private website": "buckets: {www: {website: {}}}\n",
		"index slash":     "buckets: {www: {public: true, website: {index: /index.html}}}\n",
		"error dotdot":    "buckets: {www: {public: true, website: {error: ../x.html}}}\n",
		"no origins":      "buckets: {www: {cors: {allowed-methods: [GET]}}}\n",
		"two wildcards":   "buckets: {www: {cors: {allowed-origins: [\"https://*.*.example.com\"]}}}\n",
		"bad method":      "buckets: {www: {cors: {allowed-origins: [\"*\"], allowed-methods: [PATCH]}}}\n",
		"negative age":    "buckets: {www: {cors: {allowed-origins: [\"*\"], max-age: -1s}}}\n",
		"unknown key":     "buckets: {www: {publik: true}}\n",
		"negative expiry": "buckets: {c: {expire-after: -1h}}\n",
		"negative size":   "buckets: {cache: {max-size: -1}}\n",
		"evict no size":   "buckets: {cache: {evict: true}}\n",
	} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadUnvalidated(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	t.Setenv("PLAINBUCKET_ACCESS_LOG", "true")
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:9100")
	if _, err := Load(""); err == nil {
		t.Error("Load accepted a configuration without keys")
	}
	cfg, err := LoadUnvalidated("")
	if err != nil || cfg.Address != "127.0.0.1:9100" || !cfg.AccessLog {
		t.Errorf("LoadUnvalidated = %+v, %v", cfg, err)
	}
	if _, err := LoadUnvalidated(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}
