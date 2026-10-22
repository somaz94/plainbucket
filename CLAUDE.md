# plainbucket

<br/>

## Overview

Single-node, S3-compatible object server whose storage is plain files: object `<key>` of bucket `<b>` is the file `<root>/<b>/<key>`. Split out of `static-file-server` on 2026-10-22, whose `internal/s3cache` stays a GitLab Runner cache subset and is not to grow into this.

<br/>

## Architecture

- `cmd/`, `cmd/cli/` - Cobra entry point; the root command serves.
- `internal/config/` - env (`PLAINBUCKET_*`) > YAML > defaults, `KnownFields`, malformed typed values are errors.
- `internal/sigv4/` - SigV4 header and presigned verification, aws-chunked decoder with chunk and trailer signatures. `sigv4test/` is an independent signer for tests.
- `internal/checksum/` - CRC32, CRC32C, CRC64NVME, SHA1, SHA256 on the standard library.
- `internal/storage/` - the filesystem store: key mapping, sidecar metadata, atomic commit, S3-order listing walker, multipart, janitor. Knows nothing about HTTP.
- `internal/s3api/` - HTTP router and handlers, XML documents, payload verification, storage error to S3 error mapping.
- `internal/s3err/` - S3 error type and XML rendering.
- `internal/server/` - listener lifecycle and the hourly janitor.
- `compat/` - separate Go module running the AWS SDK for Go v2 against the handler.
- `hack/test-awscli.sh` - aws cli end-to-end script against `bin/plainbucket`.

<br/>

## Invariants

- The main module depends only on the standard library, Cobra and yaml.v3. Test-only SDK use goes in `compat/`, never in the main `go.mod`.
- Objects are written to `.plainbucket/tmp`, verified, then renamed into place. Nothing writes into a bucket directory in place.
- A sidecar is trusted only while its recorded size and mtime match the data file; otherwise the object is treated as edited outside the server.
- Listing order must equal S3 byte order of whole keys. `storage/list_test.go` checks the walker against a brute-force reference; keep it passing when touching `walk`.
- Path-style addressing only. New operations return `501` until implemented, never a silent success.

<br/>

## Build and Test

```bash
make build
make test          # race detector, cross-package coverage profile
make cover-check   # coverage gate, also run in CI
make test-compat   # AWS SDK for Go v2 (compat/ module)
make test-awscli   # aws cli end-to-end
make test-all      # all of the above
make lint
```

CI (`.github/workflows/test.yml`) runs `test`, `cover-check`, `test-compat` and `test-awscli`.

<br/>

## Docs

- `docs/configuration.md` - settings table; update it with `internal/config`.
- `docs/layout.md` - disk layout, external edits, key mapping limits.
- `docs/compatibility.md` - supported and unsupported operations; update it with the router in `internal/s3api/handler.go`.
