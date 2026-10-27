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
		cfg.BodyTimeout != time.Minute || cfg.Metrics || cfg.BucketMetrics || cfg.Console || cfg.HasTLS() || cfg.ScrubInterval != 0 || cfg.ScrubRate != 32<<20 || cfg.ReplicateInterval != time.Hour {
		t.Errorf("defaults = %+v", cfg)
	}

	t.Setenv("PLAINBUCKET_ROOT", "/srv")
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:9100")
	t.Setenv("PLAINBUCKET_REGION", "eu-west-1")
	t.Setenv("PLAINBUCKET_MAX_OBJECT_SIZE", "1024")
	t.Setenv("PLAINBUCKET_MIN_FREE_SPACE", "4096")
	t.Setenv("PLAINBUCKET_UPLOAD_EXPIRY", "2h")
	t.Setenv("PLAINBUCKET_BODY_TIMEOUT", "0s")
	t.Setenv("PLAINBUCKET_METRICS", "true")
	t.Setenv("PLAINBUCKET_BUCKET_METRICS", "true")
	t.Setenv("PLAINBUCKET_CONSOLE", "1")
	t.Setenv("PLAINBUCKET_READ_ONLY", "yes")
	t.Setenv("PLAINBUCKET_SCRUB_INTERVAL", "720h")
	t.Setenv("PLAINBUCKET_SCRUB_RATE", "1048576")
	t.Setenv("PLAINBUCKET_LOG_FORMAT", "JSON")
	t.Setenv("PLAINBUCKET_DEBUG", "yes")
	t.Setenv("PLAINBUCKET_TLS_CERT", "c.pem")
	t.Setenv("PLAINBUCKET_TLS_KEY", "k.pem")
	t.Setenv("PLAINBUCKET_TLS_MIN_VERS", "tls13")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "/srv" || cfg.Address != "127.0.0.1:9100" || cfg.Region != "eu-west-1" || cfg.MaxObjectSize != 1024 || cfg.MinFreeSpace != 4096 ||
		cfg.UploadExpiry != 2*time.Hour || cfg.BodyTimeout != 0 || !cfg.Metrics || !cfg.BucketMetrics || !cfg.Console || !cfg.ReadOnly || cfg.ScrubInterval != 720*time.Hour || cfg.ScrubRate != 1<<20 || cfg.LogFormat != "json" || !cfg.Debug || !cfg.HasTLS() || cfg.TLSMinVersion() != tls.VersionTLS13 {
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
		"no keys":         {"PLAINBUCKET_ACCESS_KEY": ""},
		"no root":         {"PLAINBUCKET_ROOT": ""},
		"no address":      {"PLAINBUCKET_ADDRESS": ""},
		"no region":       {"PLAINBUCKET_REGION": ""},
		"negative size":   {"PLAINBUCKET_MAX_OBJECT_SIZE": "-1"},
		"negative free":   {"PLAINBUCKET_MIN_FREE_SPACE": "-1"},
		"zero expiry":     {"PLAINBUCKET_UPLOAD_EXPIRY": "0s"},
		"negative body":   {"PLAINBUCKET_BODY_TIMEOUT": "-1s"},
		"negative scrub":  {"PLAINBUCKET_SCRUB_INTERVAL": "-1h"},
		"replicate often": {"PLAINBUCKET_REPLICATE_INTERVAL": "30s"},
		"negative rate":   {"PLAINBUCKET_SCRUB_RATE": "-1"},
		"half tls":        {"PLAINBUCKET_TLS_CERT": "c.pem"},
		"tls version":     {"PLAINBUCKET_TLS_MIN_VERS": "TLS10"},
		"log format":      {"PLAINBUCKET_LOG_FORMAT": "xml"},
		"bad bool":        {"PLAINBUCKET_DEBUG": "maybe"},
		"bad duration":    {"PLAINBUCKET_UPLOAD_EXPIRY": "7d"},
		"bad size":        {"PLAINBUCKET_MAX_OBJECT_SIZE": "10Gi"},
		"half key":        {"PLAINBUCKET_SECRET_KEY": ""},
		"public bucket":   {"PLAINBUCKET_PUBLIC_BUCKETS": "ok-bucket,Bad_Name"},
		"creds missing":   {"PLAINBUCKET_CREDENTIALS_FILE": "/nonexistent/creds.yaml"},
		"domain port":     {"PLAINBUCKET_DOMAINS": "s3.example.com:9000"},
		"domain scheme":   {"PLAINBUCKET_DOMAINS": "https://s3.example.com"},
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
  docs:
    trash-after: 168h
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
	if cfg.Buckets["docs"].TrashAfter != 168*time.Hour {
		t.Errorf("docs = %+v", cfg.Buckets["docs"])
	}
	if !cfg.Buckets["assets"].Public {
		t.Error("PLAINBUCKET_PUBLIC_BUCKETS not merged")
	}

	secret := filepath.Join(dir, "hook-secret")
	_ = os.WriteFile(secret, []byte("k3y\n"), 0o600)
	_ = os.WriteFile(path, []byte(`buckets:
  uploads:
    notify:
      - url: https://hooks.example.com/in
        events: ["s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"]
        prefix: in/
        secret-file: `+secret+`
      - {id: audit, url: "http://audit.example.com:8080/"}
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if n := cfg.Buckets["uploads"].Notify; len(n) != 2 || n[0].ID != "notify-1" || string(n[0].Secret) != "k3y" ||
		n[0].Prefix != "in/" || len(n[0].Events) != 2 || n[1].ID != "audit" || n[1].Secret != nil {
		t.Errorf("notify = %+v", n)
	}
	_ = os.WriteFile(path, []byte(`buckets:
  www:
    headers:
      - set: {cache-control: "max-age=60", x-frame-options: DENY}
      - {suffix: .html, set: {Cache-Control: no-cache}}
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if h := cfg.Buckets["www"].Headers; len(h) != 2 || h[0].Set["Cache-Control"] != "max-age=60" || h[0].Set["X-Frame-Options"] != "DENY" ||
		h[1].Suffix != ".html" || h[1].Set["Cache-Control"] != "no-cache" {
		t.Errorf("headers = %+v", h)
	}
	_ = os.WriteFile(path, []byte(`buckets:
  ci-cache:
    expire-after: 336h
    lifecycle:
      - {prefix: logs/, delete-after: 168h}
      - {prefix: tmp/, expire-after: 24h}
      - {prefix: keep/}
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if lc := cfg.Buckets["ci-cache"].Lifecycle; len(lc) != 3 || lc[0].DeleteAfter != 168*time.Hour || lc[1].ExpireAfter != 24*time.Hour || lc[2].Prefix != "keep/" {
		t.Errorf("lifecycle = %+v", lc)
	}
	_ = os.WriteFile(path, []byte(`buckets:
  uploads:
    replicate:
      - {endpoint: "https://s3.example.com/", bucket: backup, prefix: in/, access-key: AKX, secret-key-file: `+secret+`, delete: true}
      - {id: dr, endpoint: "http://192.0.2.10:9000", region: eu-west-1, bucket: dr-copy, virtual-hosted: true, access-key: B, secret-key-file: `+secret+`}
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if r := cfg.Buckets["uploads"].Replicate; len(r) != 2 || r[0].ID != "replicate-1" || r[0].Endpoint != "https://s3.example.com" ||
		r[0].Region != "us-east-1" || r[0].SecretKey != "k3y" || !r[0].Delete || r[0].Prefix != "in/" ||
		r[1].ID != "dr" || r[1].Region != "eu-west-1" || !r[1].VirtualHosted {
		t.Errorf("replicate = %+v", r)
	}
	empty := filepath.Join(dir, "empty-secret")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)

	for name, body := range map[string]string{
		"notify no url":            "buckets: {uploads: {notify: [{events: [\"s3:ObjectCreated:*\"]}]}}\n",
		"notify bad scheme":        "buckets: {uploads: {notify: [{url: \"ftp://h/x\"}]}}\n",
		"notify no host":           "buckets: {uploads: {notify: [{url: \"http:///x\"}]}}\n",
		"notify bad event":         "buckets: {uploads: {notify: [{url: \"http://h\", events: [\"s3:ObjectTagging:*\"]}]}}\n",
		"notify same id":           "buckets: {uploads: {notify: [{id: a, url: \"http://h\"}, {id: a, url: \"http://g\"}]}}\n",
		"notify no secret":         "buckets: {uploads: {notify: [{url: \"http://h\", secret-file: " + filepath.Join(dir, "missing") + "}]}}\n",
		"notify empty secret":      "buckets: {uploads: {notify: [{url: \"http://h\", secret-file: " + empty + "}]}}\n",
		"notify inline secret":     "buckets: {uploads: {notify: [{url: \"http://h\", secret: x}]}}\n",
		"replicate bad scheme":     "buckets: {uploads: {replicate: [{endpoint: \"ftp://h\", bucket: b-1, access-key: a, secret-key-file: " + secret + "}]}}\n",
		"replicate path":           "buckets: {uploads: {replicate: [{endpoint: \"https://h/base\", bucket: b-1, access-key: a, secret-key-file: " + secret + "}]}}\n",
		"replicate bad bucket":     "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: B, access-key: a, secret-key-file: " + secret + "}]}}\n",
		"replicate no key":         "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, secret-key-file: " + secret + "}]}}\n",
		"replicate no secret":      "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, access-key: a}]}}\n",
		"replicate missing secret": "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, access-key: a, secret-key-file: " + filepath.Join(dir, "missing") + "}]}}\n",
		"replicate empty secret":   "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, access-key: a, secret-key-file: " + empty + "}]}}\n",
		"replicate inline secret":  "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, access-key: a, secret-key: x}]}}\n",
		"replicate overlapping":    "buckets: {uploads: {replicate: [{endpoint: \"https://h\", bucket: b-1, prefix: a/, access-key: a, secret-key-file: " + secret + "}]}, videos: {replicate: [{endpoint: \"https://h\", bucket: b-1, access-key: a, secret-key-file: " + secret + "}]}}\n",
		"replicate same id":        "buckets: {uploads: {replicate: [{id: x, endpoint: \"https://h\", bucket: b-1, access-key: a, secret-key-file: " + secret + "}, {id: x, endpoint: \"https://g\", bucket: b-1, access-key: a, secret-key-file: " + secret + "}]}}\n",
		"headers empty set":        "buckets: {www: {headers: [{prefix: a/}]}}\n",
		"lifecycle twice":          "buckets: {ci-cache: {lifecycle: [{prefix: a/, expire-after: 1h}, {prefix: a/, delete-after: 1h}]}}\n",
		"lifecycle negative":       "buckets: {ci-cache: {lifecycle: [{prefix: a/, delete-after: -1h}]}}\n",
		"lifecycle bad key":        "buckets: {ci-cache: {lifecycle: [{prefix: a/, expire: 1h}]}}\n",
		"headers content type":     "buckets: {www: {headers: [{set: {Content-Type: text/html}}]}}\n",
		"headers csp":              "buckets: {www: {headers: [{set: {Content-Security-Policy: none}}]}}\n",
		"headers twice":            "buckets: {www: {headers: [{set: {cache-control: a, Cache-Control: b}}]}}\n",
		"headers empty value":      "buckets: {www: {headers: [{set: {Cache-Control: \"\"}}]}}\n",
		"headers newline":          "buckets: {www: {headers: [{set: {Cache-Control: \"a\\nb\"}}]}}\n",
		"headers blank":            "buckets: {www: {headers: [{set: {Cache-Control: \" \"}}]}}\n",
		"headers non-ascii":        "buckets: {www: {headers: [{set: {Content-Disposition: \"é\"}}]}}\n",
		"headers too long":         "buckets: {www: {headers: [{set: {Cache-Control: " + strings.Repeat("a", 4097) + "}}]}}\n",
	} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
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
		"negative expiry": "buckets: {cold: {expire-after: -1h}}\n",
		"negative trash":  "buckets: {cold: {trash-after: -1h}}\n",
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

func TestRateLimits(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	_ = os.WriteFile(path, []byte(`rate-limit: {requests: 50, burst: 100, bytes: 10485760}
anonymous-rate-limit: {requests: 5}
credentials:
  - {access-key: ci, secret-key: ci-secret, rate-limit: {requests: 1000}}
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.RateLimit; r == nil || r.Requests != 50 || r.Burst != 100 || r.Bytes != 10<<20 {
		t.Errorf("rate-limit = %+v", r)
	}
	if r := cfg.AnonymousRateLimit; r == nil || r.Requests != 5 {
		t.Errorf("anonymous-rate-limit = %+v", r)
	}
	if c := cfg.Credentials[0]; c.RateLimit == nil || c.RateLimit.Requests != 1000 {
		t.Errorf("credential rate-limit = %+v", c.RateLimit)
	}
	for name, body := range map[string]string{
		"negative requests":  "rate-limit: {requests: -1}\n",
		"negative anonymous": "anonymous-rate-limit: {bytes: -1}\n",
		"negative key":       "credentials: [{access-key: ci, secret-key: s, rate-limit: {burst: -1}}]\n",
		"unknown field":      "rate-limit: {rps: 5}\n",
	} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRateLimitBounds(t *testing.T) {
	setRequired(t)
	path := filepath.Join(t.TempDir(), "c.yaml")
	for name, body := range map[string]string{
		"nan requests":     "rate-limit: {requests: .nan}\n",
		"infinite":         "rate-limit: {requests: .inf}\n",
		"bytes below body": "rate-limit: {bytes: 100}\n", // 32 KiB per 1m body-timeout needs about 546
	} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Without a body timeout any byte rate is possible.
	t.Setenv("PLAINBUCKET_BODY_TIMEOUT", "0")
	_ = os.WriteFile(path, []byte("rate-limit: {bytes: 100}\n"), 0o600)
	if _, err := Load(path); err != nil {
		t.Errorf("slow rate without a body timeout: %v", err)
	}
}

func TestOneYAMLDocument(t *testing.T) {
	setRequired(t)
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("region: eu-west-1\n---\nregion: us-west-2\n"), 0o600)
	if _, err := Load(path); err == nil || err.Error() != "loading config file: only one YAML document is allowed" {
		t.Errorf("second document: %v", err)
	}
	_ = os.WriteFile(path, []byte("region: eu-west-1\n---\n"), 0o600)
	if cfg, err := Load(path); err != nil || cfg.Region != "eu-west-1" {
		t.Errorf("trailing empty document: %v", err)
	}
}
