# plainbucket

An S3-compatible object server that stores every object as a plain file.

Bucket `photos` is the directory `<root>/photos`, and object `2026/trip.jpg` in it is the file `<root>/photos/2026/trip.jpg`. Nothing is chunked, encoded or hidden in a database, so the data stays readable with `ls`, `cp` and `rsync` when the server is stopped, and files you drop into the directory show up as objects.

plainbucket is a single binary with no runtime dependencies beyond the Go standard library, Cobra and a YAML parser. It is meant for one node: a laptop, a homelab box, or a Kubernetes pod with one volume. It does not replicate, erasure-code or version objects.

<br/>

## Features

- **S3 API, path-style and virtual-hosted**: buckets, objects, `ListObjectsV2`, `ListObjects` and `ListObjectVersions`, multipart upload with composite and full-object checksums, reads by part number, `GetObjectAttributes`, `CopyObject`, `DeleteObjects`, ranged and conditional reads, conditional writes (`If-None-Match`, `If-Match`). See [docs/compatibility.md](docs/compatibility.md).
- **Web console**: browse, upload, download, share by presigned URL and check bucket usage from a browser; the page signs requests itself, so access keys keep their limits. See [docs/configuration.md](docs/configuration.md#web-console).
- **Access control**: several access keys, each optionally limited to some buckets or to reads, and public buckets whose objects anyone can fetch. See [docs/configuration.md](docs/configuration.md#credentials).
- **Bucket limits**: per-bucket expiry by last use and a size cap that either refuses writes or evicts the least recently used objects, so a CI cache bucket looks after itself. See [docs/configuration.md](docs/configuration.md#expiry-and-size-limits).
- **Static websites**: a public bucket can serve index and error documents and answer CORS preflights, so a site or a browser app needs no other web server. See [docs/configuration.md](docs/configuration.md#buckets).
- **AWS Signature Version 4**: `Authorization` header and presigned URLs, every payload mode the AWS SDKs send, including aws-chunked streaming with trailing checksums.
- **Checksums**: CRC32, CRC32C, CRC64NVME, SHA-1 and SHA-256 are verified on upload and returned on request. An upload that declares none gets a CRC64NVME, as on S3.
- **Plain files**: objects are files, metadata lives in a separate system directory, and files changed outside the server are detected and re-hashed. See [docs/layout.md](docs/layout.md).
- **Tested with real clients**: the official AWS SDK for Go v2, the aws cli, rclone, the `gitlab-runner` cache helper and a subset of the ceph/s3-tests conformance suite run against the server in CI. See [docs/gitlab-runner.md](docs/gitlab-runner.md) for using it as a runner cache.
- **Operations**: Prometheus metrics, an access log, a `/healthz` endpoint with a `healthcheck` command for the shell-less image, an offline `fsck`, and protection against uploads that stall.

<br/>

## Quick Start

```bash
make build

PLAINBUCKET_ROOT=./data \
PLAINBUCKET_ACCESS_KEY=admin \
PLAINBUCKET_SECRET_KEY=change-me \
./bin/plainbucket
```

Point any S3 client at it:

```bash
export AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=change-me AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:9000 s3 mb s3://site
aws --endpoint-url http://localhost:9000 s3 sync ./public s3://site
ls ./data/site            # the synced files, as files
```

With Docker:

```bash
docker run -d -p 9000:9000 \
  -e PLAINBUCKET_ACCESS_KEY=admin -e PLAINBUCKET_SECRET_KEY=change-me \
  -v "$PWD/data:/data" somaz940/plainbucket:latest
```

On Kubernetes, use the Helm chart:

```bash
helm repo add plainbucket https://somaz94.github.io/plainbucket/helm-repo
helm install plainbucket plainbucket/plainbucket --set auth.existingSecret=plainbucket-auth
```

See [helm/plainbucket/README.md](helm/plainbucket/README.md) for what it assumes and the example values.

The image runs as UID 65532. A directory mounted at `/data` must be writable by that user (`chown 65532 data`), or start the container with `--user` set to the directory's owner.

SDKs need path-style addressing for a custom endpoint, unless `PLAINBUCKET_DOMAINS` and wildcard DNS are set up for virtual-hosted style. In the AWS SDK for Go v2:

```go
client := s3.NewFromConfig(cfg, func(o *s3.Options) {
	o.BaseEndpoint = aws.String("http://localhost:9000")
	o.UsePathStyle = true
})
```

<br/>

## Configuration

Settings come from environment variables (prefix `PLAINBUCKET_`), then an optional YAML file passed with `-c`, then defaults. At least one access key is required. See [docs/configuration.md](docs/configuration.md).

<br/>

## Serving the Same Files over HTTP

Because buckets are directories, a static file server can publish them read-only while clients write through the S3 API. With [static-file-server](https://github.com/somaz94/static-file-server), set `FOLDER` to the plainbucket root and `HIDE_DOT_FILES=true`, so the `.plainbucket` system directory stays private.

<br/>

## Limitations

The filesystem layout trades some S3 semantics for plain files:

- A key and a longer key using it as a directory cannot coexist (`a` and `a/b`). The second write fails with `409 KeyConflict`.
- Keys must map to valid paths: no empty, `.` or `..` segments, and no segment longer than 255 bytes.
- On a case-insensitive filesystem, keys differing only in case are the same object. The server logs a warning at startup when it detects one.
- Keys are stored byte for byte, so whether the NFC and NFD spellings of a name are one object depends on the filesystem.
- Deleting the last object under a prefix also removes the now-empty directories, unless one was created as a folder marker (`a/`).
- Symlinks are followed only while they stay inside the root.

[docs/layout.md](docs/layout.md) explains each of these.

<br/>

## Development

```bash
make help          # every target
make test          # unit tests with the race detector and a coverage profile
make cover-check   # fail below the coverage minimum
make test-compat   # AWS SDK for Go v2 suite (separate module under compat/)
make test-awscli   # aws cli end-to-end script
make test-rclone   # rclone end-to-end script
make test-runner   # gitlab-runner cache end-to-end script (downloads a pinned gitlab-runner)
make test-console  # web console end-to-end script (needs node)
make test-s3tests  # ceph/s3-tests subset (needs python3; pinned and cached in bin/)
make test-helm     # Helm chart lint, renders and schema checks
make lint
```

`compat/` is its own Go module so the AWS SDK never becomes a dependency of the server.

<br/>

## License

Apache License 2.0. See [LICENSE](LICENSE).
