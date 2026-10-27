package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/somaz94/plainbucket/internal/storage"
)

// validateReplicaTargets refuses two rules writing overlapping keys of one target bucket:
// each would undo the other, and one with delete would remove the other's copies.
func validateReplicaTargets(buckets map[string]Bucket) error {
	type rule struct{ where, target, prefix string }
	var rules []rule
	for _, name := range slices.Sorted(maps.Keys(buckets)) {
		for i, r := range buckets[name].Replicate {
			rules = append(rules, rule{fmt.Sprintf("buckets.%s.replicate[%d]", name, i), r.Endpoint + "/" + r.Bucket, r.Prefix})
		}
	}
	for i, a := range rules {
		for _, b := range rules[i+1:] {
			if a.target == b.target && (strings.HasPrefix(a.prefix, b.prefix) || strings.HasPrefix(b.prefix, a.prefix)) {
				return fmt.Errorf("%s and %s write overlapping keys of %s", a.where, b.where, a.target)
			}
		}
	}
	return nil
}

func validateReplicate(where string, rules []Replicate) error {
	ids := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		at := fmt.Sprintf("%s.replicate[%d]", where, i)
		if r.ID == "" {
			r.ID = fmt.Sprintf("replicate-%d", i+1)
		}
		if ids[r.ID] {
			return fmt.Errorf("%s: id %q is used twice", at, r.ID)
		}
		ids[r.ID] = true
		if u, err := url.Parse(r.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return fmt.Errorf("%s.endpoint: %q must be an http or https URL with no path, such as https://s3.example.com", at, r.Endpoint)
		}
		r.Endpoint = strings.TrimSuffix(r.Endpoint, "/")
		if r.Region == "" {
			r.Region = "us-east-1"
		}
		if !storage.ValidBucketName(r.Bucket) {
			return fmt.Errorf("%s.bucket: %q is not a valid bucket name", at, r.Bucket)
		}
		if r.AccessKey == "" || r.SecretKeyFile == "" {
			return fmt.Errorf("%s: access-key and secret-key-file are required", at)
		}
		raw, err := os.ReadFile(r.SecretKeyFile)
		if err != nil {
			return fmt.Errorf("%s.secret-key-file: %w", at, err)
		}
		if r.SecretKey = strings.TrimRight(string(raw), "\r\n"); r.SecretKey == "" {
			return fmt.Errorf("%s.secret-key-file: %s is empty", at, r.SecretKeyFile)
		}
	}
	return nil
}
