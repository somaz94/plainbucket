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
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix prefixes every environment variable.
const EnvPrefix = "PLAINBUCKET_"

// Config holds the server configuration.
type Config struct {
	Root          string        `yaml:"root"`
	Address       string        `yaml:"address"`
	AccessKey     string        `yaml:"access-key"`
	SecretKey     string        `yaml:"secret-key"`
	Region        string        `yaml:"region"`
	MaxObjectSize int64         `yaml:"max-object-size"`
	UploadExpiry  time.Duration `yaml:"upload-expiry"`
	LogFormat     string        `yaml:"log-format"`
	Debug         bool          `yaml:"debug"`
	TLSCert       string        `yaml:"tls-cert"`
	TLSKey        string        `yaml:"tls-key"`
	TLSMinVers    string        `yaml:"tls-min-vers"`
}

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		Root:         "/data",
		Address:      ":9000",
		Region:       "us-east-1",
		UploadExpiry: 7 * 24 * time.Hour,
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
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}

func (c *Config) loadYAML(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	// A misspelled key would otherwise be dropped silently.
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
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
	e.string("REGION", &c.Region)
	e.int64("MAX_OBJECT_SIZE", &c.MaxObjectSize)
	e.duration("UPLOAD_EXPIRY", &c.UploadExpiry)
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
	case c.AccessKey == "" || c.SecretKey == "":
		return errors.New("PLAINBUCKET_ACCESS_KEY and PLAINBUCKET_SECRET_KEY are required")
	case c.Region == "":
		return errors.New("PLAINBUCKET_REGION must not be empty")
	case c.MaxObjectSize < 0:
		return errors.New("PLAINBUCKET_MAX_OBJECT_SIZE must not be negative")
	case c.UploadExpiry <= 0:
		return errors.New("PLAINBUCKET_UPLOAD_EXPIRY must be positive")
	case (c.TLSCert == "") != (c.TLSKey == ""):
		return errors.New("PLAINBUCKET_TLS_CERT and PLAINBUCKET_TLS_KEY must be set together")
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
		slog.String("region", c.Region),
		slog.Int64("max_object_size", c.MaxObjectSize),
		slog.Duration("upload_expiry", c.UploadExpiry),
		slog.String("log_format", c.LogFormat),
		slog.Bool("debug", c.Debug),
		slog.Bool("tls", c.HasTLS()),
		slog.String("tls_min_vers", c.TLSMinVers),
	)
}
