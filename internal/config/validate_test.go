package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somaz94/plainbucket/internal/httpx"
)

// Buckets and header names are checked in sorted order, so each body has one expected error.
func TestValidationMessages(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	secret := filepath.Join(dir, "secret")
	empty := filepath.Join(dir, "empty")
	missing := filepath.Join(dir, "missing")
	_ = os.WriteFile(secret, []byte("k3y\n"), 0o600)
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	rep := func(fields string) string {
		return "buckets: {uploads: {replicate: [{" + fields + "}]}}\n"
	}
	const target = `endpoint: "https://h", bucket: b-1, access-key: a, secret-key-file: `

	for _, tc := range []struct{ name, body, want string }{
		{"bad name", "buckets: {Bad_Name: {public: true}}\n", `buckets.Bad_Name: "Bad_Name" is not a valid bucket name`},
		{"private website", "buckets: {www: {website: {}}}\n", "buckets.www.website requires public: true"},
		{"index slash", "buckets: {www: {public: true, website: {index: /index.html}}}\n",
			`buckets.www.website: "/index.html" must be an object key, without a leading or trailing /`},
		{"error dotdot", "buckets: {www: {public: true, website: {error: ../x.html}}}\n",
			`buckets.www.website: "../x.html" must be an object key, without a leading or trailing /`},
		{"negative expiry", "buckets: {cold: {expire-after: -1h}}\n", "buckets.cold.expire-after must not be negative"},
		{"negative trash", "buckets: {cold: {trash-after: -1h}}\n", "buckets.cold.trash-after must not be negative"},
		{"negative size", "buckets: {cache: {max-size: -1}}\n", "buckets.cache.max-size must not be negative"},
		{"evict no size", "buckets: {cache: {evict: true}}\n", "buckets.cache.evict needs max-size"},
		{"lifecycle twice", "buckets: {ci-cache: {lifecycle: [{prefix: a/, expire-after: 1h}, {prefix: a/, delete-after: 1h}]}}\n",
			`buckets.ci-cache.lifecycle[1]: prefix "a/" has a rule already`},
		{"lifecycle negative", "buckets: {ci-cache: {lifecycle: [{prefix: a/, delete-after: -1h}]}}\n",
			"buckets.ci-cache.lifecycle[0]: expire-after and delete-after must not be negative"},
		{"no origins", "buckets: {www: {cors: {allowed-methods: [GET]}}}\n", "buckets.www.cors.allowed-origins must not be empty"},
		{"two wildcards", "buckets: {www: {cors: {allowed-origins: [\"https://*.*.example.com\"]}}}\n",
			`buckets.www.cors.allowed-origins: "https://*.*.example.com" may hold at most one *`},
		{"bad method", "buckets: {www: {cors: {allowed-origins: [\"*\"], allowed-methods: [patch]}}}\n",
			`buckets.www.cors.allowed-methods: "patch" must be one of GET, HEAD, PUT, POST, DELETE`},
		{"negative age", "buckets: {www: {cors: {allowed-origins: [\"*\"], max-age: -1s}}}\n", "buckets.www.cors.max-age must not be negative"},
		{"notify same id", "buckets: {uploads: {notify: [{id: a, url: \"http://h\"}, {id: a, url: \"http://g\"}]}}\n",
			`buckets.uploads.notify[1]: id "a" is used twice`},
		{"notify bad scheme", "buckets: {uploads: {notify: [{url: \"ftp://h/x\"}]}}\n", `buckets.uploads.notify[0].url: "ftp://h/x" must be an http or https URL`},
		{"notify bad event", "buckets: {uploads: {notify: [{url: \"http://h\", events: [\"s3:ObjectTagging:*\"]}]}}\n",
			`buckets.uploads.notify[0].events: "s3:ObjectTagging:*" is not a supported event, such as s3:ObjectCreated:* or s3:ObjectRemoved:Delete`},
		{"notify missing secret", "buckets: {uploads: {notify: [{url: \"http://h\", secret-file: " + missing + "}]}}\n",
			"buckets.uploads.notify[0].secret-file: open " + missing + ": no such file or directory"},
		{"notify empty secret", "buckets: {uploads: {notify: [{url: \"http://h\", secret-file: " + empty + "}]}}\n",
			"buckets.uploads.notify[0].secret-file: " + empty + " is empty"},
		{"replicate same id", "buckets: {uploads: {replicate: [{id: x, " + target + secret + "}, {id: x, " + target + secret + "}]}}\n",
			`buckets.uploads.replicate[1]: id "x" is used twice`},
		{"replicate path", rep(`endpoint: "https://h/base", bucket: b-1, access-key: a, secret-key-file: ` + secret),
			`buckets.uploads.replicate[0].endpoint: "https://h/base" must be an http or https URL with no path, such as https://s3.example.com`},
		{"replicate bad bucket", rep(`endpoint: "https://h", bucket: B, access-key: a, secret-key-file: ` + secret),
			`buckets.uploads.replicate[0].bucket: "B" is not a valid bucket name`},
		{"replicate no key", rep(`endpoint: "https://h", bucket: b-1, secret-key-file: ` + secret),
			"buckets.uploads.replicate[0]: access-key and secret-key-file are required"},
		{"replicate missing secret", rep(target + missing),
			"buckets.uploads.replicate[0].secret-key-file: open " + missing + ": no such file or directory"},
		{"replicate empty secret", rep(target + empty), "buckets.uploads.replicate[0].secret-key-file: " + empty + " is empty"},
		{"replicate overlapping", "buckets: {uploads: {replicate: [{prefix: a/, " + target + secret + "}]}, videos: {replicate: [{" + target + secret + "}]}}\n",
			"buckets.uploads.replicate[0] and buckets.videos.replicate[0] write overlapping keys of https://h/b-1"},
		{"headers empty set", "buckets: {www: {headers: [{prefix: a/}]}}\n", "buckets.www.headers[0].set must name at least one header"},
		{"headers not allowed", "buckets: {www: {headers: [{set: {content-type: text/html}}]}}\n",
			`buckets.www.headers[0].set: "content-type" may not be set; allowed: ` + strings.Join(httpx.RuleHeaders, ", ")},
		{"headers twice", "buckets: {www: {headers: [{set: {cache-control: a, Cache-Control: b}}]}}\n",
			`buckets.www.headers[0].set: "Cache-Control" is given twice`},
		{"headers bad value", "buckets: {www: {headers: [{set: {Cache-Control: \"a\\nb\"}}]}}\n",
			"buckets.www.headers[0].set.Cache-Control must be printable ASCII, not blank, at most 4096 bytes"},
		{"bad domain", "domains: [\"s3.example.com:9000\"]\n",
			`PLAINBUCKET_DOMAINS: "s3.example.com:9000" must be a host name without scheme or port, such as s3.example.com`},
		{"buckets before domains", "domains: [\"s3.example.com:9000\"]\nbuckets: {cold: {expire-after: -1h}}\n",
			"buckets.cold.expire-after must not be negative"},
		{"first bad bucket by name", "buckets: {zeta: {expire-after: -1h}, mid: {trash-after: -1h}, delta: {evict: true}, beta: {max-size: -2}, alpha: {max-size: -1}}\n",
			"buckets.alpha.max-size must not be negative"},
		{"first bad header by name", "buckets: {www: {headers: [{set: {X-Custom: a, X-Other: b, Server: c, Link: d, Cache-Control: \"\"}}]}}\n",
			"buckets.www.headers[0].set.Cache-Control must be printable ASCII, not blank, at most 4096 bytes"},
		// Each row breaks two neighbouring checks; the earlier one must be reported.
		{"rate limit before buckets", "rate-limit: {requests: .nan}\nbuckets: {cold: {expire-after: -1h}}\n",
			"rate-limit: requests, burst and bytes must be finite and not negative"},
		{"name before website", "buckets: {Bad_Name: {website: {}}}\n", `buckets.Bad_Name: "Bad_Name" is not a valid bucket name`},
		{"website before limits", "buckets: {cold: {website: {}, expire-after: -1h}}\n", "buckets.cold.website requires public: true"},
		{"limits before notify", "buckets: {cold: {expire-after: -1h, notify: [{url: \"ftp://h\"}]}}\n",
			"buckets.cold.expire-after must not be negative"},
		{"notify before replicate", "buckets: {cold: {notify: [{url: \"ftp://h\"}], replicate: [{endpoint: \"ftp://h\"}]}}\n",
			`buckets.cold.notify[0].url: "ftp://h" must be an http or https URL`},
		{"replicate before headers", "buckets: {cold: {replicate: [{endpoint: \"ftp://h\"}], headers: [{prefix: a/}]}}\n",
			`buckets.cold.replicate[0].endpoint: "ftp://h" must be an http or https URL with no path, such as https://s3.example.com`},
		{"headers before lifecycle", "buckets: {cold: {headers: [{prefix: a/}], lifecycle: [{prefix: a/, delete-after: -1h}]}}\n",
			"buckets.cold.headers[0].set must name at least one header"},
		{"lifecycle before cors", "buckets: {cold: {lifecycle: [{prefix: a/, delete-after: -1h}], cors: {}}}\n",
			"buckets.cold.lifecycle[0]: expire-after and delete-after must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.WriteFile(path, []byte(tc.body), 0o600)
			_, err := Load(path)
			if want := "config validation: " + tc.want; err == nil || err.Error() != want {
				t.Errorf("error = %v\nwant    %s", err, want)
			}
		})
	}
}
