# plainbucket

<br/>

## Overview

Single-node, S3-compatible object server whose storage is plain files: object `<key>` of bucket `<b>` is the file `<root>/<b>/<key>`. Split out of `static-file-server` on 2026-10-22, whose `internal/s3cache` stays a GitLab Runner cache subset and is not to grow into this.

<br/>

## Architecture

- `cmd/`, `cmd/cli/` - Cobra entry point; the root command serves, plus `healthcheck` (Docker HEALTHCHECK, no access key needed) and `fsck` (offline check and repair; `--verify` re-reads the data, as the server's periodic scrub does).
- `internal/config/` - env (`PLAINBUCKET_*`) > YAML > defaults, `KnownFields`, malformed typed values are errors.
- `internal/sigv4/` - SigV4 header and presigned verification, aws-chunked decoder with chunk and trailer signatures. `sigv4test/` is an independent signer for tests.
- `internal/checksum/` - CRC32, CRC32C, CRC64NVME, SHA1, SHA256 on the standard library.
- `internal/storage/` - the filesystem store: key mapping, sidecar metadata, atomic commit, S3-order listing walker, multipart, janitor. Knows nothing about HTTP.
- `internal/s3api/` - HTTP router and handlers, access keys with bucket scopes and read-only, public-bucket anonymous reads, virtual-hosted routing, XML documents, payload verification and upload progress deadline, storage error to S3 error mapping, Prometheus metrics.
- `internal/s3err/` - S3 error type and XML rendering.
- `internal/notify/` - bucket event webhooks: S3 event JSON, HMAC signature, a bounded queue and retries per target, swapped whole by `SetTargets` on reload.
- `internal/console/` - the embedded web console (`static/`: vanilla JS, its own SigV4 signer in `sigv4.js` and stored-ZIP writer in `zip.js`); `s3api` routes `/_console/` to it and serves the signed usage and trash APIs.
- `internal/server/` - listener lifecycle, the hourly janitor, and configuration reloads on `SIGHUP` or a changed config, credentials, certificate or webhook secret file.
- `compat/` - separate Go module running the AWS SDK for Go v2 against the handler.
- `helm/plainbucket/` - Helm chart; `hack/test-helm.sh` lints it, renders it and checks the fail guards and schema. Published by `.github/workflows/helm-release.yml` on tags.
- `hack/test-awscli.sh`, `hack/test-rclone.sh`, `hack/test-gitlab-runner.sh`, `hack/test-console.sh` (with `hack/test-console.mjs`) - client end-to-end scripts against `bin/plainbucket`; `hack/e2e-lib.sh` starts the server for them. The runner script downloads the official binary at the version and SHA-256 pinned in it and caches it in `bin/`; `compat/cmd/presign` makes its presigned URLs.
- `hack/test-s3tests.sh` - ceph/s3-tests subset at a pinned commit with frozen `hack/s3tests-requirements.txt`, kept in `bin/`; the run must keep every test in `hack/s3tests-pass.txt` passing.

<br/>

## Invariants

- The main module depends only on the standard library, Cobra and yaml.v3. Test-only SDK use goes in `compat/`, never in the main `go.mod`.
- Objects are written to `.plainbucket/tmp`, verified, then renamed into place. Nothing writes into a bucket directory in place.
- Every path built from a bucket name or key goes through the store's `os.Root`, so `..` and symlinks cannot leave the root. Do not reintroduce plain `os.Open` / `os.Rename` on data paths.
- Every `x-amz-*` request header must be signed (`sigv4.requireSigned`); the GET-for-HEAD presign fallback exists only for the GitLab Runner cache.
- A sidecar is trusted only while its recorded size and mtime match the data file; otherwise the object is treated as edited outside the server.
- Listing order must equal S3 byte order of whole keys. `storage/list_test.go` checks the walker against a brute-force reference; keep it passing when touching `walk`.
- Path-style always works; virtual-hosted style only for `PLAINBUCKET_DOMAINS`. The request URL is never rewritten, because SigV4 signs the path as sent.
- Anonymous requests may only `GET` / `HEAD` objects of public buckets (website buckets add index and error documents), plus CORS preflights on buckets with a `cors` rule; listing, multipart state, tags and writes always need a key. Bucket policy, ACL, website and CORS APIs stay `501`: behavior is configured in the `buckets` map, not set through the API.
- New operations return `501` until implemented, never a silent success.
- The console is static files that sign S3 requests in the browser; it gets no server-side session. Its pages load nothing from other origins and use no `innerHTML`, inline script or inline style, because the CSP forbids them. Only the usage and trash APIs are console-specific, both need a signed request, and restoring or purging trash are the console's only writes that do not go through the S3 API (decided 2026-10-23: S3 has no operation for them). With the console on, every other path-style response carries the `console.ObjectSandbox` CSP, so a stored page never runs in the console's origin.
- The Helm chart runs exactly one pod with `Recreate`: locks are in-process. Keep `values.schema.json` closed and in step with `values.yaml` and every `.Values` path the templates read; credentials only ever reach the pod through Secrets.
- What a reload replaces is swapped whole through atomic pointers (`Handler.SetAccess`, `Store.SetLimits`, the TLS certificate); never modify those maps in place.
- One process per root: the server and fsck take `storage.Lock` (flock on `.plainbucket/lock`), because object locks live in the process.
- Expiry and eviction go through `removeIfUnused`, which re-checks size, mtime and last use under the key lock, so a scan never deletes an object rewritten or read after it. Last use is the later of the data file's mtime (write) and the sidecar's (read); reads move only the sidecar's, never the data file's.
- A delete in a bucket with `trash-after` moves the file into `.plainbucket/trash` (`trashFile`); expiry and eviction never do. A restore goes through `account` and never replaces an object.
- Event notifications never block or fail a request: `Notifier.Publish` drops on a full queue, and handlers publish only after the store committed the change. Deletes are published only when `DeleteObjectIf` reports an object removed; expiry and eviction reach the notifier through `Store.OnLimitRemove`.
- Bucket usage changes go through `account` (writes, may refuse) or `adjust` (deletes and rollbacks, never refuse); both record into `measuring` while `MeasureUsage` walks, so its swap does not drop them.
- `hack/bump-version.sh` moves the Makefile image tag, Chart `version` / `appVersion` and `values.yaml` `image.tag` together.

<br/>

## Build and Test

```bash
make build
make test          # race detector, cross-package coverage profile
make cover-check   # coverage gate, also run in CI
make test-compat   # AWS SDK for Go v2 (compat/ module)
make test-awscli   # aws cli end-to-end
make test-rclone   # rclone end-to-end
make test-runner   # gitlab-runner cache helper end-to-end
make test-console  # console files and its JS signer against the server (node)
make test-s3tests  # ceph/s3-tests subset against hack/s3tests-pass.txt
make s3tests-update # rewrite the pass list from a run
make test-helm     # Helm chart tests
make test-all      # all of the above
make lint
```

CI (`.github/workflows/test.yml`) runs `test`, `cover-check`, `test-compat`, `test-awscli`, `test-rclone`, `test-runner`, `test-console` and `test-helm`, and `test-s3tests` as a separate job.

<br/>

## Docs

- `docs/configuration.md` - settings table and the web console; update it with `internal/config`.
- `docs/layout.md` - disk layout, external edits, key mapping limits.
- `docs/gitlab-runner.md` - runner `config.toml` for the cache, and what the runner e2e covers.
- `docs/compatibility.md` - supported and unsupported operations; update it with the router in `internal/s3api/handler.go`.
