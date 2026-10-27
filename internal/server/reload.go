package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"sync/atomic"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/replicate"
	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/storage"
)

// reloadPoll is how often the configuration, credentials, certificate and secret files
// (webhook and replication) are checked for changes. A Kubernetes Secret update reaches a mounted file within about a
// minute, so polling faster gains little.
const reloadPoll = 30 * time.Second

// reloadSource says where a new configuration comes from; the zero value never reloads.
type reloadSource struct {
	configFile string
	hup        <-chan os.Signal
	every      time.Duration
}

// certificate is the TLS key pair in force, swapped on reload.
type certificate struct {
	pair atomic.Pointer[tls.Certificate]
}

func (c *certificate) load(certFile, keyFile string) error {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	c.pair.Store(&pair)
	return nil
}

func (c *certificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) { return c.pair.Load(), nil }

// reloader takes a changed configuration over: access keys, bucket policies, limits, trash,
// webhooks and replication rules, min-free-space, the read-only mode and the certificate.
// Other settings, replicate-interval among them, are reported as needing a restart.
type reloader struct {
	src        reloadSource
	boot       *config.Config // what the server started with
	cfg        *config.Config // in force
	handler    *s3api.Handler
	store      *storage.Store
	notifier   *notify.Notifier
	replicator *replicate.Replicator
	cert       *certificate // nil without TLS
	logger     *slog.Logger
	stamps     map[string]fileStamp
	missing    string // a file the last failed reload could not read; its appearance retries
}

type fileStamp struct {
	size int64
	mod  time.Time
}

func (r *reloader) run(ctx context.Context) {
	r.stamps = r.snapshot()
	var tick <-chan time.Time
	if r.src.every > 0 {
		t := time.NewTicker(r.src.every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.src.hup:
			r.reload(ctx, "signal")
		case <-tick:
			if !maps.Equal(r.snapshot(), r.stamps) {
				r.reload(ctx, "file change")
			}
		}
	}
}

// snapshot stats the files the configuration is read from; os.Stat follows the symlinks
// Kubernetes swaps when it updates a mounted Secret.
func (r *reloader) snapshot() map[string]fileStamp {
	out := map[string]fileStamp{}
	files := []string{r.src.configFile, r.cfg.CredentialsFile, r.cfg.TLSCert, r.cfg.TLSKey, r.missing}
	for _, b := range r.cfg.Buckets {
		for _, n := range b.Notify {
			files = append(files, n.SecretFile)
		}
		for _, rp := range b.Replicate {
			files = append(files, rp.AccessKeyFile, rp.SecretKeyFile, rp.SessionTokenFile)
		}
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		var s fileStamp
		if fi, err := os.Stat(f); err == nil {
			s = fileStamp{fi.Size(), fi.ModTime()}
		}
		out[f] = s
	}
	return out
}

func (r *reloader) reload(ctx context.Context, cause string) {
	// Taken first, so a file that fails to load is not retried on every tick.
	r.missing = ""
	r.stamps = r.snapshot()
	next, err := config.Load(r.src.configFile)
	if pe, ok := errors.AsType[*fs.PathError](err); ok && errors.Is(pe, fs.ErrNotExist) {
		r.missing = pe.Path // such as a secret-file whose Secret is not mounted yet
		r.stamps[pe.Path] = fileStamp{}
	}
	if err != nil {
		r.logger.Error("reload failed; keeping the running configuration", "cause", cause, "err", err)
		return
	}
	if r.cert != nil && next.HasTLS() {
		if err := r.cert.load(next.TLSCert, next.TLSKey); err != nil {
			r.logger.Error("reload failed; keeping the running configuration", "cause", cause, "err", err)
			return
		}
	}
	if names := r.boot.RestartNeeded(next); len(names) > 0 {
		r.logger.Warn("some settings change only on restart", "settings", names)
	}
	limits := bucketLimits(next)
	if len(limits) > 0 && len(bucketLimits(r.cfg)) == 0 {
		// Usage was never measured, and a refusing limit checks writes against it.
		r.store.MeasureUsage(ctx)
	}
	r.store.SetLimits(limits)
	r.store.SetMinFree(next.MinFreeSpace)
	r.store.SetTrash(bucketTrash(next))
	if _, _, err := r.store.DiskSpace(); err != nil && next.MinFreeSpace > 0 {
		r.logger.Warn("min-free-space is not enforced: free space cannot be read", "err", err)
	}
	r.handler.SetAccess(credentials(next), bucketPolicies(next), nil)
	if rateLimit(next.AnonymousRateLimit) != rateLimit(r.cfg.AnonymousRateLimit) {
		r.handler.SetAnonymousRateLimit(rateLimit(next.AnonymousRateLimit))
	}
	if next.ReadOnly != r.cfg.ReadOnly {
		r.logger.Warn("read-only mode changed", "read_only", next.ReadOnly)
	}
	r.handler.SetReadOnly(next.ReadOnly)
	r.store.SetPaused(next.ReadOnly)
	if r.notifier != nil {
		r.notifier.SetTargets(notifyTargets(next))
	}
	if r.replicator != nil {
		r.replicator.SetRules(replicateRules(next))
	}
	r.cfg = next
	r.stamps = r.snapshot()
	r.logger.Info("configuration reloaded", "cause", cause, "keys", len(next.AllCredentials()), "buckets", len(next.Buckets))
}
