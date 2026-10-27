package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
	e.int64("MIN_FREE_SPACE", &c.MinFreeSpace)
	e.duration("UPLOAD_EXPIRY", &c.UploadExpiry)
	e.duration("SCRUB_INTERVAL", &c.ScrubInterval)
	e.duration("REPLICATE_INTERVAL", &c.ReplicateInterval)
	e.int64("SCRUB_RATE", &c.ScrubRate)
	e.duration("BODY_TIMEOUT", &c.BodyTimeout)
	e.bool("METRICS", &c.Metrics)
	e.bool("BUCKET_METRICS", &c.BucketMetrics)
	e.bool("CONSOLE", &c.Console)
	e.bool("READ_ONLY", &c.ReadOnly)
	e.string("LOG_FORMAT", &c.LogFormat)
	e.bool("DEBUG", &c.Debug)
	e.bool("ACCESS_LOG", &c.AccessLog)
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
