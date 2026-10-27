package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func secrets(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { s, ok := m[k]; return s, ok }
}

func TestIssueAndParse(t *testing.T) {
	c := Claims{Parent: "ci", Buckets: []string{"cache"}, ReadOnly: true, Name: "job-1", Expires: now.Add(time.Hour)}
	cred := Issue("ci-secret", c)
	if !strings.HasPrefix(cred.AccessKey, KeyPrefix) || len(cred.AccessKey) != 20 || len(cred.SecretKey) != 40 || cred.Token == "" {
		t.Fatalf("credentials %+v", cred)
	}
	got, again, err := Parse(cred.Token, secrets(map[string]string{"ci": "ci-secret"}), now)
	if err != nil || again != cred || got.Parent != "ci" || got.Buckets[0] != "cache" || !got.ReadOnly || got.Name != "job-1" || !got.Expires.Equal(c.Expires) {
		t.Fatalf("parse: %+v %+v %v", got, again, err)
	}
	// Another issue of the same claims is the same session; other claims are another.
	if Issue("ci-secret", c) != cred || Issue("ci-secret", Claims{Parent: "ci", Expires: c.Expires}).AccessKey == cred.AccessKey {
		t.Error("credentials not derived from the claims")
	}
}

func TestParseRejects(t *testing.T) {
	cred := Issue("ci-secret", Claims{Parent: "ci", Expires: now.Add(time.Hour)})
	body, sig, _ := strings.Cut(cred.Token, ".")
	other := Issue("ci-secret", Claims{Parent: "ci", ReadOnly: true, Expires: now.Add(time.Hour)})
	otherBody, _, _ := strings.Cut(other.Token, ".")
	for name, c := range map[string]struct {
		token   string
		secrets map[string]string
		at      time.Time
		want    error
	}{
		"no dot":         {"abc", map[string]string{"ci": "ci-secret"}, now, ErrMalformed},
		"bad body":       {"!!." + sig, map[string]string{"ci": "ci-secret"}, now, ErrMalformed},
		"not json":       {"YWJj." + sig, map[string]string{"ci": "ci-secret"}, now, ErrMalformed},
		"unknown field":  {b64.EncodeToString([]byte(`{"p":"ci","e":"2030-01-01T00:00:00Z","x":1}`)) + "." + sig, map[string]string{"ci": "ci-secret"}, now, ErrMalformed},
		"too long":       {strings.Repeat("a", MaxTokenLen) + "." + sig, map[string]string{"ci": "ci-secret"}, now, ErrMalformed},
		"unknown issuer": {cred.Token, map[string]string{}, now, ErrInvalid},
		"changed secret": {cred.Token, map[string]string{"ci": "rotated"}, now, ErrInvalid},
		"claims swapped": {otherBody + "." + sig, map[string]string{"ci": "ci-secret"}, now, ErrInvalid},
		"bad signature":  {body + ".!!", map[string]string{"ci": "ci-secret"}, now, ErrInvalid},
		"expired":        {cred.Token, map[string]string{"ci": "ci-secret"}, now.Add(time.Hour), ErrExpired},
	} {
		if _, _, err := Parse(c.token, secrets(c.secrets), c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}
