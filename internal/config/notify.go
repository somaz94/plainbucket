package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"

	"github.com/somaz94/plainbucket/internal/notify"
)

func validateNotify(where string, targets []Notify) error {
	ids := map[string]bool{}
	for i := range targets {
		n := &targets[i]
		at := fmt.Sprintf("%s.notify[%d]", where, i)
		if n.ID == "" {
			n.ID = fmt.Sprintf("notify-%d", i+1)
		}
		if ids[n.ID] {
			return fmt.Errorf("%s: id %q is used twice", at, n.ID)
		}
		ids[n.ID] = true
		if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s.url: %q must be an http or https URL", at, n.URL)
		}
		for _, e := range n.Events {
			if !notify.ValidEvent(e) {
				return fmt.Errorf("%s.events: %q is not a supported event, such as s3:ObjectCreated:* or s3:ObjectRemoved:Delete", at, e)
			}
		}
		if n.SecretFile != "" {
			raw, err := os.ReadFile(n.SecretFile)
			if err != nil {
				return fmt.Errorf("%s.secret-file: %w", at, err)
			}
			// Secrets written with echo end in a newline nobody means as part of the key.
			if n.Secret = bytes.TrimRight(raw, "\r\n"); len(n.Secret) == 0 {
				return fmt.Errorf("%s.secret-file: %s is empty", at, n.SecretFile)
			}
		}
	}
	return nil
}
