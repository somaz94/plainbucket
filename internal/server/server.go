// Package server runs the S3 API listener and the storage janitor.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/s3api"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	// Objects can be gigabytes, so only the header read is bounded.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownWait      = 10 * time.Second
	janitorInterval   = time.Hour
)

// NewLogger returns the process logger: cfg.LogFormat picks text or JSON, cfg.Debug
// enables request logs and the startup configuration.
func NewLogger(w io.Writer, cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if cfg.Debug {
		opts.Level = slog.LevelDebug
	}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg *config.Config) error {
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, NewLogger(os.Stderr, cfg), ln)
}

func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger, ln net.Listener) error {
	logger.Debug("configuration", "config", cfg)
	store, err := storage.New(cfg.Root)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("opening %s: %w", cfg.Root, err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Lock(); errors.Is(err, storage.ErrLocked) {
		_ = ln.Close()
		return fmt.Errorf("%s: %w", cfg.Root, err)
	} else if err != nil {
		logger.Warn("could not lock the root; make sure no other plainbucket uses it", "err", err)
	}
	store.SetLimits(bucketLimits(cfg))
	if store.CaseInsensitive() {
		logger.Warn("the root filesystem is case-insensitive: keys differing only in case refer to the same file", "root", store.Root())
	}

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	var janitor sync.WaitGroup
	janitor.Go(func() { store.RunJanitor(janitorCtx, janitorInterval, cfg.UploadExpiry, logger) })
	defer func() {
		stopJanitor()
		janitor.Wait()
	}()

	srv := &http.Server{
		Handler: s3api.New(s3api.Options{
			Store:         store,
			Credentials:   credentials(cfg),
			Buckets:       bucketPolicies(cfg),
			Domains:       cfg.Domains,
			Region:        cfg.Region,
			MaxObjectSize: cfg.MaxObjectSize,
			Logger:        logger,
			Metrics:       cfg.Metrics,
			AccessLog:     cfg.AccessLog || cfg.Debug,
			BodyTimeout:   cfg.BodyTimeout,
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	scheme := "http"
	if cfg.HasTLS() {
		scheme = "https"
		srv.TLSConfig = &tls.Config{MinVersion: cfg.TLSMinVersion()}
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("serving", "root", store.Root(), "scheme", scheme, "addr", ln.Addr().String())
		if cfg.HasTLS() {
			errCh <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down", "cause", context.Cause(ctx))
		sctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close() // cut the requests still running rather than leave them writing
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("server stopped")
		return nil
	}
}

func credentials(cfg *config.Config) []s3api.Credential {
	var out []s3api.Credential
	for _, c := range cfg.AllCredentials() {
		out = append(out, s3api.Credential{AccessKey: c.AccessKey, SecretKey: c.SecretKey, Buckets: c.Buckets, ReadOnly: c.ReadOnly})
	}
	return out
}

func bucketPolicies(cfg *config.Config) map[string]s3api.BucketPolicy {
	out := map[string]s3api.BucketPolicy{}
	for name, b := range cfg.Buckets {
		p := s3api.BucketPolicy{Public: b.Public}
		if w := b.Website; w != nil {
			p.Website = &s3api.Website{Index: w.Index, Error: w.Error}
		}
		if c := b.CORS; c != nil {
			p.CORS = &s3api.CORS{AllowedOrigins: c.AllowedOrigins, AllowedMethods: c.AllowedMethods,
				AllowedHeaders: c.AllowedHeaders, ExposeHeaders: c.ExposeHeaders, MaxAge: c.MaxAge}
		}
		out[name] = p
	}
	return out
}

func bucketLimits(cfg *config.Config) map[string]storage.Limit {
	out := map[string]storage.Limit{}
	for name, b := range cfg.Buckets {
		if b.ExpireAfter > 0 || b.MaxSize > 0 {
			out[name] = storage.Limit{ExpireAfter: b.ExpireAfter, MaxSize: b.MaxSize, Evict: b.Evict}
		}
	}
	return out
}
