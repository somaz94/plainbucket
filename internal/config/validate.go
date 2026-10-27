package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/storage"
)

// check validates r against the body timeout: a byte rate so low that a read waits past
// it would cut uploads off instead of slowing them.
func (r *RateLimit) check(what string, bodyTimeout time.Duration) error {
	if r == nil {
		return nil
	}
	if r.Requests < 0 || math.IsNaN(r.Requests) || math.IsInf(r.Requests, 0) || r.Burst < 0 || r.Bytes < 0 {
		return fmt.Errorf("%s: requests, burst and bytes must be finite and not negative", what)
	}
	if secs := int64(bodyTimeout / time.Second); secs > 0 && r.Bytes > 0 {
		if floor := int64(httpx.MinBodyProgress) / secs; r.Bytes < floor {
			return fmt.Errorf("%s: bytes must be at least %d with body-timeout %s", what, floor, bodyTimeout)
		}
	}
	return nil
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
	case c.MinFreeSpace < 0:
		return errors.New("PLAINBUCKET_MIN_FREE_SPACE must not be negative")
	case c.UploadExpiry <= 0:
		return errors.New("PLAINBUCKET_UPLOAD_EXPIRY must be positive")
	case c.ScrubInterval < 0:
		return errors.New("PLAINBUCKET_SCRUB_INTERVAL must not be negative")
	case c.ReplicateInterval < time.Minute:
		return errors.New("PLAINBUCKET_REPLICATE_INTERVAL must be at least 1m")
	case c.ScrubRate < 0:
		return errors.New("PLAINBUCKET_SCRUB_RATE must not be negative")
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
		if err := cr.RateLimit.check("credential "+cr.AccessKey+" rate-limit", c.BodyTimeout); err != nil {
			return err
		}
	}
	if err := checkBuckets("PLAINBUCKET_PUBLIC_BUCKETS", c.PublicBuckets); err != nil {
		return err
	}
	if err := errors.Join(c.RateLimit.check("rate-limit", c.BodyTimeout), c.AnonymousRateLimit.check("anonymous-rate-limit", c.BodyTimeout)); err != nil {
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

func checkBuckets(what string, names []string) error {
	for _, b := range names {
		if !storage.ValidBucketName(b) {
			return fmt.Errorf("%s: %q is not a valid bucket name", what, b)
		}
	}
	return nil
}
