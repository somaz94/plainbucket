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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/somaz94/plainbucket/internal/storage"
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
	// Buckets holds per-bucket behavior; PublicBuckets marks entries public.
	Buckets map[string]Bucket `yaml:"buckets"`
	// Domains enable virtual-hosted-style requests: Host <bucket>.<domain>.
	Domains       []string      `yaml:"domains"`
	Region        string        `yaml:"region"`
	MaxObjectSize int64         `yaml:"max-object-size"`
	UploadExpiry  time.Duration `yaml:"upload-expiry"`
	BodyTimeout   time.Duration `yaml:"body-timeout"`
	Metrics       bool          `yaml:"metrics"`
	LogFormat     string        `yaml:"log-format"`
	Debug         bool          `yaml:"debug"`
	TLSCert       string        `yaml:"tls-cert"`
	TLSKey        string        `yaml:"tls-key"`
	TLSMinVers    string        `yaml:"tls-min-vers"`
}

// Credential is one access key. Buckets limits it to those buckets (empty = all) and
// ReadOnly to GET and HEAD.
type Credential struct {
	AccessKey string   `yaml:"access-key"`
	SecretKey string   `yaml:"secret-key"`
	Buckets   []string `yaml:"buckets"`
	ReadOnly  bool     `yaml:"read-only"`
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
		Root:         "/data",
		Address:      ":9000",
		Region:       "us-east-1",
		UploadExpiry: 7 * 24 * time.Hour,
		BodyTimeout:  time.Minute,
		LogFormat:    "text",
	}
}

// Load applies defaults, then the YAML file (if configFile is not empty), then env vars.
func Load(configFile string) (*Config, error) {
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
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
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
	return nil
}

func (c *Config) loadEnv() error {
	var e envBinder
	e.string("ROOT", &c.Root)
	e.string("ADDRESS", &c.Address)
	e.string("ACCESS_KEY", &c.AccessKey)
	e.string("SECRET_KEY", &c.SecretKey)
	e.string("CREDENTIALS_FILE", &c.CredentialsFile)
	e.list("PUBLIC_BUCKETS", &c.PublicBuckets)
	e.list("DOMAINS", &c.Domains)
	e.string("REGION", &c.Region)
	e.int64("MAX_OBJECT_SIZE", &c.MaxObjectSize)
	e.duration("UPLOAD_EXPIRY", &c.UploadExpiry)
	e.duration("BODY_TIMEOUT", &c.BodyTimeout)
	e.bool("METRICS", &c.Metrics)
	e.string("LOG_FORMAT", &c.LogFormat)
	e.bool("DEBUG", &c.Debug)
	e.string("TLS_CERT", &c.TLSCert)
	e.string("TLS_KEY", &c.TLSKey)
	e.string("TLS_MIN_VERS", &c.TLSMinVers)
	return errors.Join(e.errs...)
}

// envBinder applies environment variables to typed fields and collects parse errors.
// An empty typed value counts as unset, as compose files render ${VAR} for a missing VAR.
type envBinder struct {
	errs []error
}

func (e *envBinder) string(key string, dst *string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		*dst = v
	}
}

// list reads a comma-separated value, dropping empty items.
func (e *envBinder) list(key string, dst *[]string) {
	raw, ok := e.lookup(key)
	if !ok {
		return
	}
	var out []string
	for v := range strings.SplitSeq(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	*dst = out
}

func (e *envBinder) lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(EnvPrefix + key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (e *envBinder) fail(key, raw, want string) {
	e.errs = append(e.errs, fmt.Errorf("%s%s=%q: must be %s", EnvPrefix, key, raw, want))
}

func (e *envBinder) bool(key string, dst *bool) {
	raw, ok := e.lookup(key)
	if !ok {
		return
	}
	switch strings.ToLower(raw) {
	case "1", "true", "t", "yes", "y":
		*dst = true
	case "0", "false", "f", "no", "n":
		*dst = false
	default:
		e.fail(key, raw, "a boolean (true/t/yes/y/1 or false/f/no/n/0)")
	}
}

func (e *envBinder) duration(key string, dst *time.Duration) {
	raw, ok := e.lookup(key)
	if !ok {
		return
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		e.fail(key, raw, "a Go duration such as 168h")
		return
	}
	*dst = v
}

func (e *envBinder) int64(key string, dst *int64) {
	raw, ok := e.lookup(key)
	if !ok {
		return
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		e.fail(key, raw, "a whole number of bytes")
		return
	}
	*dst = v
}

func (c *Config) validate() error {
	switch {
	case c.Root == "":
		return errors.New("PLAINBUCKET_ROOT is required")
	case c.Address == "":
		return errors.New("PLAINBUCKET_ADDRESS is required")
	case (c.AccessKey == "") != (c.SecretKey == ""):
		return errors.New("PLAINBUCKET_ACCESS_KEY and PLAINBUCKET_SECRET_KEY must be set together")
	case len(c.AllCredentials()) == 0:
		return errors.New("no access key: set PLAINBUCKET_ACCESS_KEY and PLAINBUCKET_SECRET_KEY, or credentials")
	case c.Region == "":
		return errors.New("PLAINBUCKET_REGION must not be empty")
	case c.MaxObjectSize < 0:
		return errors.New("PLAINBUCKET_MAX_OBJECT_SIZE must not be negative")
	case c.UploadExpiry <= 0:
		return errors.New("PLAINBUCKET_UPLOAD_EXPIRY must be positive")
	case c.BodyTimeout < 0:
		return errors.New("PLAINBUCKET_BODY_TIMEOUT must not be negative")
	case (c.TLSCert == "") != (c.TLSKey == ""):
		return errors.New("PLAINBUCKET_TLS_CERT and PLAINBUCKET_TLS_KEY must be set together")
	}
	if err := c.validateAccess(); err != nil {
		return err
	}
	if _, ok := tlsVersions[normalizeTLS(c.TLSMinVers)]; c.TLSMinVers != "" && !ok {
		return fmt.Errorf("invalid PLAINBUCKET_TLS_MIN_VERS %q: must be TLS12 or TLS13", c.TLSMinVers)
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
		c.LogFormat = strings.ToLower(c.LogFormat)
	default:
		return fmt.Errorf("PLAINBUCKET_LOG_FORMAT must be \"text\" or \"json\", got %q", c.LogFormat)
	}
	return nil
}

func (c *Config) validateAccess() error {
	seen := map[string]bool{}
	for i, cr := range c.AllCredentials() {
		switch {
		case cr.AccessKey == "" || cr.SecretKey == "":
			return fmt.Errorf("credential %d: access-key and secret-key are required", i)
		case seen[cr.AccessKey]:
			return fmt.Errorf("access key %q is defined twice", cr.AccessKey)
		}
		seen[cr.AccessKey] = true
		if err := checkBuckets("credential "+cr.AccessKey+" buckets", cr.Buckets); err != nil {
			return err
		}
	}
	if err := checkBuckets("PLAINBUCKET_PUBLIC_BUCKETS", c.PublicBuckets); err != nil {
		return err
	}
	if err := c.validateBuckets(); err != nil {
		return err
	}
	for i, d := range c.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if !domainRe.MatchString(d) {
			return fmt.Errorf("PLAINBUCKET_DOMAINS: %q must be a host name without scheme or port, such as s3.example.com", c.Domains[i])
		}
		c.Domains[i] = d
	}
	return nil
}

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

var corsMethods = []string{"GET", "HEAD", "PUT", "POST", "DELETE"}

// validateBuckets merges PublicBuckets into Buckets and checks every entry.
func (c *Config) validateBuckets() error {
	if c.Buckets == nil {
		c.Buckets = map[string]Bucket{}
	}
	for _, name := range c.PublicBuckets {
		b := c.Buckets[name]
		b.Public = true
		c.Buckets[name] = b
	}
	for name, b := range c.Buckets {
		where := "buckets." + name
		if !storage.ValidBucketName(name) {
			return fmt.Errorf("%s: %q is not a valid bucket name", where, name)
		}
		if w := b.Website; w != nil {
			if !b.Public {
				return fmt.Errorf("%s.website requires public: true", where)
			}
			if w.Index == "" {
				w.Index = "index.html"
			}
			for _, doc := range []string{w.Index, w.Error} {
				if strings.HasPrefix(doc, "/") || strings.HasSuffix(doc, "/") {
					return fmt.Errorf("%s.website: %q must be an object key, without a leading or trailing /", where, doc)
				}
			}
		}
		switch {
		case b.ExpireAfter < 0:
			return fmt.Errorf("%s.expire-after must not be negative", where)
		case b.MaxSize < 0:
			return fmt.Errorf("%s.max-size must not be negative", where)
		case b.Evict && b.MaxSize == 0:
			return fmt.Errorf("%s.evict needs max-size", where)
		}
		if cr := b.CORS; cr != nil {
			if len(cr.AllowedOrigins) == 0 {
				return fmt.Errorf("%s.cors.allowed-origins must not be empty", where)
			}
			for _, o := range cr.AllowedOrigins {
				if strings.Count(o, "*") > 1 {
					return fmt.Errorf("%s.cors.allowed-origins: %q may hold at most one *", where, o)
				}
			}
			for i, m := range cr.AllowedMethods {
				cr.AllowedMethods[i] = strings.ToUpper(m)
				if !slices.Contains(corsMethods, cr.AllowedMethods[i]) {
					return fmt.Errorf("%s.cors.allowed-methods: %q must be one of %s", where, m, strings.Join(corsMethods, ", "))
				}
			}
			if cr.MaxAge < 0 {
				return fmt.Errorf("%s.cors.max-age must not be negative", where)
			}
		}
	}
	return nil
}

func checkBuckets(what string, names []string) error {
	for _, b := range names {
		if !storage.ValidBucketName(b) {
			return fmt.Errorf("%s: %q is not a valid bucket name", what, b)
		}
	}
	return nil
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
		slog.Int("configured_buckets", len(c.Buckets)),
		slog.Any("domains", c.Domains),
		slog.String("region", c.Region),
		slog.Int64("max_object_size", c.MaxObjectSize),
		slog.Duration("upload_expiry", c.UploadExpiry),
		slog.Duration("body_timeout", c.BodyTimeout),
		slog.Bool("metrics", c.Metrics),
		slog.String("log_format", c.LogFormat),
		slog.Bool("debug", c.Debug),
		slog.Bool("tls", c.HasTLS()),
		slog.String("tls_min_vers", c.TLSMinVers),
	)
}
