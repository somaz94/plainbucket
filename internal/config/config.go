// Package config loads configuration from environment variables, a YAML file and
// defaults. Priority: env vars > YAML > defaults.
package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix prefixes every environment variable.
const EnvPrefix = "PLAINBUCKET_"

// Config holds the server configuration.
type Config struct {
	Root      string `yaml:"root"`
	Address   string `yaml:"address"`
	AccessKey string `yaml:"access-key"`
	SecretKey string `yaml:"secret-key"`
	// Credentials are keys beyond AccessKey; CredentialsFile adds more from a YAML list,
	// so a Kubernetes Secret can hold them without the rest of the configuration.
	Credentials     []Credential `yaml:"credentials"`
	CredentialsFile string       `yaml:"credentials-file"`
	PublicBuckets   []string     `yaml:"public-buckets"`
	// RateLimit applies to every key without its own; AnonymousRateLimit to unsigned
	// requests, per client address.
	RateLimit          *RateLimit `yaml:"rate-limit"`
	AnonymousRateLimit *RateLimit `yaml:"anonymous-rate-limit"`
	// Buckets holds per-bucket behavior; PublicBuckets marks entries public.
	Buckets map[string]Bucket `yaml:"buckets"`
	// Domains enable virtual-hosted-style requests: Host <bucket>.<domain>.
	Domains       []string      `yaml:"domains"`
	Region        string        `yaml:"region"`
	MaxObjectSize int64         `yaml:"max-object-size"`
	MinFreeSpace  int64         `yaml:"min-free-space"`
	UploadExpiry  time.Duration `yaml:"upload-expiry"`
	// ScrubInterval re-reads every object this often to find damaged data; 0 turns it off.
	ScrubInterval time.Duration `yaml:"scrub-interval"`
	// ReplicateInterval is how often each replication rule compares the two listings.
	ReplicateInterval time.Duration `yaml:"replicate-interval"`
	ScrubRate         int64         `yaml:"scrub-rate"` // bytes per second the scrub may read; 0 for no cap
	BodyTimeout       time.Duration `yaml:"body-timeout"`
	Metrics           bool          `yaml:"metrics"`
	// BucketMetrics reports usage for every bucket, not only those with limits.
	BucketMetrics bool `yaml:"bucket-metrics"`
	Console       bool `yaml:"console"`
	// ReadOnly refuses every write and pauses the background removals, for maintenance.
	ReadOnly   bool   `yaml:"read-only"`
	LogFormat  string `yaml:"log-format"`
	Debug      bool   `yaml:"debug"`
	AccessLog  bool   `yaml:"access-log"`
	TLSCert    string `yaml:"tls-cert"`
	TLSKey     string `yaml:"tls-key"`
	TLSMinVers string `yaml:"tls-min-vers"`
}

// Credential is one access key. Buckets limits it to those buckets (empty = all) and
// ReadOnly to GET and HEAD.
type Credential struct {
	AccessKey string     `yaml:"access-key"`
	SecretKey string     `yaml:"secret-key"`
	Buckets   []string   `yaml:"buckets"`
	ReadOnly  bool       `yaml:"read-only"`
	RateLimit *RateLimit `yaml:"rate-limit"`
}

// RateLimit caps a client: Requests per second, Burst of them at once (default Requests
// rounded up), and request and response body Bytes per second; zero Requests or Bytes
// does not limit.
type RateLimit struct {
	Requests float64 `yaml:"requests"`
	Burst    int     `yaml:"burst"`
	Bytes    int64   `yaml:"bytes"`
}

// Bucket is the configured behavior of one bucket.
type Bucket struct {
	Public  bool     `yaml:"public"`
	Website *Website `yaml:"website"`
	CORS    *CORS    `yaml:"cors"`
	// ExpireAfter removes objects neither written nor read for this long.
	ExpireAfter time.Duration `yaml:"expire-after"`
	// MaxSize caps the bucket in bytes: writes past it are refused, or with Evict the
	// least recently used objects are removed.
	MaxSize int64 `yaml:"max-size"`
	Evict   bool  `yaml:"evict"`
	// TrashAfter keeps deleted objects restorable from the console for this long.
	TrashAfter time.Duration `yaml:"trash-after"`
	Notify     []Notify      `yaml:"notify"`
	Headers    []HeaderRule  `yaml:"headers"`
	Lifecycle  []Lifecycle   `yaml:"lifecycle"`
	Replicate  []Replicate   `yaml:"replicate"`
}

// Replicate copies the bucket's objects under Prefix to Bucket at Endpoint, another S3
// server, under the same keys.
type Replicate struct {
	ID            string `yaml:"id"` // default replicate-<n>
	Endpoint      string `yaml:"endpoint"`
	Region        string `yaml:"region"` // default us-east-1
	Bucket        string `yaml:"bucket"`
	Prefix        string `yaml:"prefix"`
	VirtualHosted bool   `yaml:"virtual-hosted"`
	AccessKey     string `yaml:"access-key"`
	// AccessKeyFile holds the access key instead, for temporary credentials refreshed in
	// place; validation reads it into AccessKey.
	AccessKeyFile string `yaml:"access-key-file"`
	// SecretKeyFile holds the secret key (e.g. a mounted Secret); validation reads it into SecretKey.
	SecretKeyFile string `yaml:"secret-key-file"`
	SecretKey     string `yaml:"-"`
	// SessionTokenFile, optional, holds the session token of temporary credentials;
	// validation reads it into SessionToken.
	SessionTokenFile string `yaml:"session-token-file"`
	SessionToken     string `yaml:"-"`
	// Delete removes target keys whose source key is gone.
	Delete bool `yaml:"delete"`
}

// Lifecycle overrides the bucket's ExpireAfter for the objects under Prefix; the longest
// matching prefix wins. ExpireAfter counts from the last use, DeleteAfter from the last
// write, and a rule with neither exempts its prefix from expiry.
type Lifecycle struct {
	Prefix      string        `yaml:"prefix"`
	ExpireAfter time.Duration `yaml:"expire-after"`
	DeleteAfter time.Duration `yaml:"delete-after"`
}

// HeaderRule sets default response headers on the objects whose keys match; later rules win.
type HeaderRule struct {
	Prefix string            `yaml:"prefix"`
	Suffix string            `yaml:"suffix"`
	Set    map[string]string `yaml:"set"`
}

// Notify posts the bucket's events to a webhook.
type Notify struct {
	ID     string   `yaml:"id"` // default notify-<n>
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"` // such as s3:ObjectCreated:*; empty = all
	Prefix string   `yaml:"prefix"`
	Suffix string   `yaml:"suffix"`
	// SecretFile holds the HMAC key (e.g. a mounted Secret); validation reads it into Secret.
	SecretFile string `yaml:"secret-file"`
	Secret     []byte `yaml:"-"`
}

// Website serves a public bucket as a static site to unsigned requests.
type Website struct {
	Index string `yaml:"index"` // default index.html
	Error string `yaml:"error"`
}

// CORS lists what browsers on other origins may do with a bucket.
type CORS struct {
	AllowedOrigins []string      `yaml:"allowed-origins"`
	AllowedMethods []string      `yaml:"allowed-methods"` // default GET, HEAD
	AllowedHeaders []string      `yaml:"allowed-headers"`
	ExposeHeaders  []string      `yaml:"expose-headers"`
	MaxAge         time.Duration `yaml:"max-age"`
}

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		Root:              "/data",
		Address:           ":9000",
		Region:            "us-east-1",
		UploadExpiry:      7 * 24 * time.Hour,
		ScrubRate:         32 << 20,
		ReplicateInterval: time.Hour,
		BodyTimeout:       time.Minute,
		LogFormat:         "text",
	}
}

// Load applies defaults, then the YAML file (if configFile is not empty), then env vars,
// and validates the result.
func Load(configFile string) (*Config, error) {
	cfg, err := LoadUnvalidated(configFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}

// LoadUnvalidated is Load without validation, for commands that need only the root or
// the address and must work where the access keys are not visible.
func LoadUnvalidated(configFile string) (*Config, error) {
	cfg := Default()
	if configFile != "" {
		if err := cfg.loadYAML(configFile); err != nil {
			return nil, fmt.Errorf("loading config file: %w", err)
		}
	}
	if err := cfg.loadEnv(); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	if cfg.CredentialsFile != "" {
		var extra []Credential
		if err := decodeYAML(cfg.CredentialsFile, &extra); err != nil {
			return nil, fmt.Errorf("loading credentials file: %w", err)
		}
		cfg.Credentials = append(cfg.Credentials, extra...)
	}
	return cfg, nil
}

func (c *Config) loadYAML(path string) error { return decodeYAML(path, c) }

func decodeYAML(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	// A misspelled key would otherwise be dropped silently.
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	// Later documents would be ignored as silently, so only empty ones may follow.
	for {
		var extra any
		if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if extra != nil {
			return errors.New("only one YAML document is allowed")
		}
	}
}

// AllCredentials returns every access key: AccessKey first, with full access, then
// Credentials.
func (c *Config) AllCredentials() []Credential {
	var all []Credential
	if c.AccessKey != "" {
		all = append(all, Credential{AccessKey: c.AccessKey, SecretKey: c.SecretKey})
	}
	return append(all, c.Credentials...)
}

var tlsVersions = map[string]uint16{
	"TLS12": tls.VersionTLS12,
	"TLS13": tls.VersionTLS13,
}

func normalizeTLS(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// TLSMinVersion returns the minimum TLS version, TLS 1.2 when unset.
func (c *Config) TLSMinVersion() uint16 {
	if v, ok := tlsVersions[normalizeTLS(c.TLSMinVers)]; ok {
		return v
	}
	return tls.VersionTLS12
}

// HasTLS reports whether TLS is configured.
func (c *Config) HasTLS() bool { return c.TLSCert != "" && c.TLSKey != "" }

// LogValue implements slog.LogValuer; credentials are reported only as set or unset.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("root", c.Root),
		slog.String("address", c.Address),
		slog.Bool("access_key", c.AccessKey != ""),
		slog.Bool("secret_key", c.SecretKey != ""),
		slog.Int("access_keys", len(c.AllCredentials())),
		slog.Any("public_buckets", c.PublicBuckets),
		slog.Bool("rate_limit", c.RateLimit != nil),
		slog.Bool("anonymous_rate_limit", c.AnonymousRateLimit != nil),
		slog.Int("configured_buckets", len(c.Buckets)),
		slog.Any("domains", c.Domains),
		slog.String("region", c.Region),
		slog.Int64("max_object_size", c.MaxObjectSize),
		slog.Int64("min_free_space", c.MinFreeSpace),
		slog.Duration("upload_expiry", c.UploadExpiry),
		slog.Duration("scrub_interval", c.ScrubInterval),
		slog.Duration("replicate_interval", c.ReplicateInterval),
		slog.Int64("scrub_rate", c.ScrubRate),
		slog.Duration("body_timeout", c.BodyTimeout),
		slog.Bool("metrics", c.Metrics),
		slog.Bool("bucket_metrics", c.BucketMetrics),
		slog.Bool("console", c.Console),
		slog.Bool("read_only", c.ReadOnly),
		slog.String("log_format", c.LogFormat),
		slog.Bool("debug", c.Debug),
		slog.Bool("access_log", c.AccessLog),
		slog.Bool("tls", c.HasTLS()),
		slog.String("tls_min_vers", c.TLSMinVers),
	)
}

// reloadable are the settings, by YAML name, that a running server takes over when it
// reloads; the certificate files are re-read too, but switching TLS on or off is not.
var reloadable = map[string]bool{
	"access-key": true, "secret-key": true, "credentials": true, "credentials-file": true,
	"public-buckets": true, "buckets": true, "rate-limit": true, "anonymous-rate-limit": true, "min-free-space": true, "read-only": true, "tls-cert": true, "tls-key": true,
}

// RestartNeeded lists the settings, by YAML name, that differ in next and take effect
// only when the server restarts.
func (c *Config) RestartNeeded(next *Config) []string {
	var out []string
	a, b := reflect.ValueOf(c).Elem(), reflect.ValueOf(next).Elem()
	for i := range a.NumField() {
		name, _, _ := strings.Cut(a.Type().Field(i).Tag.Get("yaml"), ",")
		if !reloadable[name] && !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			out = append(out, name)
		}
	}
	if c.HasTLS() != next.HasTLS() {
		out = append(out, "tls-cert")
	}
	return out
}
