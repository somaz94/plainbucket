package config

import (
	"fmt"
	"maps"
	"net/textproto"
	"slices"
	"strings"

	"github.com/somaz94/plainbucket/internal/httpx"
	"github.com/somaz94/plainbucket/internal/storage"
)

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
	// In name order, so the error reported for several bad buckets is always the same one.
	for _, name := range slices.Sorted(maps.Keys(c.Buckets)) {
		if err := validateBucket(name, c.Buckets[name]); err != nil {
			return err
		}
	}
	return validateReplicaTargets(c.Buckets)
}

// validateBucket checks one entry. b is a copy, but its pointers and slices share
// storage with c.Buckets, so in-place rewrites stick; a value field set here would not.
func validateBucket(name string, b Bucket) error {
	where := "buckets." + name
	if !storage.ValidBucketName(name) {
		return fmt.Errorf("%s: %q is not a valid bucket name", where, name)
	}
	if err := validateWebsite(where, b.Public, b.Website); err != nil {
		return err
	}
	switch {
	case b.ExpireAfter < 0:
		return fmt.Errorf("%s.expire-after must not be negative", where)
	case b.TrashAfter < 0:
		return fmt.Errorf("%s.trash-after must not be negative", where)
	case b.MaxSize < 0:
		return fmt.Errorf("%s.max-size must not be negative", where)
	case b.Evict && b.MaxSize == 0:
		return fmt.Errorf("%s.evict needs max-size", where)
	}
	if err := validateNotify(where, b.Notify); err != nil {
		return err
	}
	if err := validateReplicate(where, b.Replicate); err != nil {
		return err
	}
	if err := validateHeaders(where, b.Headers); err != nil {
		return err
	}
	if err := validateLifecycle(where, b.Lifecycle); err != nil {
		return err
	}
	return validateCORS(where, b.CORS)
}

// validateWebsite fills in the default index document.
func validateWebsite(where string, public bool, w *Website) error {
	if w == nil {
		return nil
	}
	if !public {
		return fmt.Errorf("%s.website requires public: true", where)
	}
	if w.Index == "" {
		w.Index = "index.html"
	}
	for _, doc := range []string{w.Index, w.Error} {
		if doc != "" && (!storage.ValidKey(doc) || strings.HasPrefix(doc, "/") || strings.HasSuffix(doc, "/")) {
			return fmt.Errorf("%s.website: %q must be an object key, without a leading or trailing /", where, doc)
		}
	}
	return nil
}

func validateLifecycle(where string, rules []Lifecycle) error {
	prefixes := map[string]bool{}
	for i, r := range rules {
		at := fmt.Sprintf("%s.lifecycle[%d]", where, i)
		switch {
		case prefixes[r.Prefix]:
			return fmt.Errorf("%s: prefix %q has a rule already", at, r.Prefix)
		case r.ExpireAfter < 0 || r.DeleteAfter < 0:
			return fmt.Errorf("%s: expire-after and delete-after must not be negative", at)
		}
		prefixes[r.Prefix] = true
	}
	return nil
}

// validateCORS rewrites the allowed methods in upper case.
func validateCORS(where string, cr *CORS) error {
	if cr == nil {
		return nil
	}
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
	return nil
}

const maxHeaderValue = 4096

// validateHeaders checks each rule and rewrites its header names in canonical form.
func validateHeaders(where string, rules []HeaderRule) error {
	for i := range rules {
		r := &rules[i]
		at := fmt.Sprintf("%s.headers[%d]", where, i)
		if len(r.Set) == 0 {
			return fmt.Errorf("%s.set must name at least one header", at)
		}
		set := make(map[string]string, len(r.Set))
		for _, name := range slices.Sorted(maps.Keys(r.Set)) {
			v := r.Set[name]
			canon := textproto.CanonicalMIMEHeaderKey(name)
			if !slices.Contains(httpx.RuleHeaders, canon) {
				return fmt.Errorf("%s.set: %q may not be set; allowed: %s", at, name, strings.Join(httpx.RuleHeaders, ", "))
			}
			if _, dup := set[canon]; dup {
				return fmt.Errorf("%s.set: %q is given twice", at, canon)
			}
			// Proxies such as ingress-nginx refuse response headers past a few KiB.
			if strings.TrimSpace(v) == "" || len(v) > maxHeaderValue || strings.ContainsFunc(v, func(c rune) bool { return c != '\t' && (c < 0x20 || c > 0x7e) }) {
				return fmt.Errorf("%s.set.%s must be printable ASCII, not blank, at most %d bytes", at, canon, maxHeaderValue)
			}
			set[canon] = v
		}
		r.Set = set
	}
	return nil
}
