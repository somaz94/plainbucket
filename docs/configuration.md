# Configuration

Each setting can be given as an environment variable or in a YAML file passed with `-c` / `--config`. An environment variable wins over the file, and the file wins over the default. Unknown YAML keys and malformed typed values (`PLAINBUCKET_UPLOAD_EXPIRY=7d`) are startup errors, not silently ignored.

<br/>

## Settings

| Environment variable | YAML key | Default | Meaning |
|---|---|---|---|
| `PLAINBUCKET_ROOT` | `root` | `/data` | Directory holding the buckets |
| `PLAINBUCKET_ADDRESS` | `address` | `:9000` | Listen address, `host:port` |
| `PLAINBUCKET_ACCESS_KEY` | `access-key` | | Access key with full access; see [Credentials](#credentials) |
| `PLAINBUCKET_SECRET_KEY` | `secret-key` | | Its secret key |
| | `credentials` | | More access keys, each optionally limited; see [Credentials](#credentials) |
| `PLAINBUCKET_CREDENTIALS_FILE` | `credentials-file` | | YAML file holding a `credentials` list, added to the others |
| `PLAINBUCKET_PUBLIC_BUCKETS` | `public-buckets` | | Comma-separated buckets whose objects anyone may read; see [Public Buckets](#public-buckets) |
| `PLAINBUCKET_DOMAINS` | `domains` | | Comma-separated domains for virtual-hosted-style requests; see [Virtual-Hosted Addressing](#virtual-hosted-addressing) |
| `PLAINBUCKET_REGION` | `region` | `us-east-1` | Region reported by `GetBucketLocation` and `HeadBucket` |
| `PLAINBUCKET_MAX_OBJECT_SIZE` | `max-object-size` | `0` | Largest object or part in bytes; `0` means no limit |
| `PLAINBUCKET_UPLOAD_EXPIRY` | `upload-expiry` | `168h` | Age after which an unfinished multipart upload is removed |
| `PLAINBUCKET_BODY_TIMEOUT` | `body-timeout` | `1m` | Cut an upload that delivers less than 32 KiB in this long; `0` disables it |
| `PLAINBUCKET_METRICS` | `metrics` | `false` | Serve Prometheus metrics at `GET /metrics` |
| `PLAINBUCKET_LOG_FORMAT` | `log-format` | `text` | `text` or `json` |
| `PLAINBUCKET_DEBUG` | `debug` | `false` | Log every request and the startup configuration |
| `PLAINBUCKET_TLS_CERT` | `tls-cert` | | Certificate file; set with `PLAINBUCKET_TLS_KEY` to serve HTTPS |
| `PLAINBUCKET_TLS_KEY` | `tls-key` | | Private key file |
| `PLAINBUCKET_TLS_MIN_VERS` | `tls-min-vers` | `TLS12` | Minimum TLS version: `TLS12` or `TLS13` |

An empty value for a typed variable counts as unset, so `PLAINBUCKET_DEBUG=` in a compose file keeps the default.

<br/>

## Example File

```yaml
root: /srv/objects
address: 127.0.0.1:9000
access-key: admin
secret-key: change-me
upload-expiry: 72h
log-format: json
```

<br/>

## Credentials

Requests are signed with AWS Signature Version 4, in the `Authorization` header or as a presigned URL; Signature Version 2 is refused. At least one access key is required. `PLAINBUCKET_ACCESS_KEY` and `PLAINBUCKET_SECRET_KEY` give one key full access. `credentials` adds more, and each can be limited:

```yaml
credentials:
  - access-key: gitlab-runner
    secret-key: change-me-too
    buckets: [runner-cache]   # only these buckets; omit for all
  - access-key: backup-reader
    secret-key: change-me-three
    read-only: true           # GET and HEAD only
```

A key limited to some buckets can create and delete only those buckets and lists only them in `ListBuckets`, and `CopyObject` also requires access to the source bucket. A request outside a key's limits gets `403 AccessDenied`. Access keys must be unique.

`PLAINBUCKET_CREDENTIALS_FILE` names a file holding only such a list (the entries without the `credentials:` line), so the keys can live in a Kubernetes Secret while the rest of the configuration stays in environment variables. Its keys are added to those from the other sources.

Unsigned requests get `403 AccessDenied`, except object reads from a public bucket and `/healthz`, plus `/metrics` when enabled.

The region in a request's credential scope is not checked against `PLAINBUCKET_REGION`, so clients configured for any region work.

<br/>

## Public Buckets

Objects in a bucket named in `PLAINBUCKET_PUBLIC_BUCKETS` can be fetched by anyone with an unsigned `GET` or `HEAD`, as with a bucket policy granting `s3:GetObject` to everyone. That is enough to host a static site or downloads straight from plainbucket. Listing the bucket, multipart state and every write still need a key, and an unsigned request may not use the `response-*` overrides, as on S3. Signed requests are unaffected.

The setting is the only way to make a bucket public: the bucket policy and ACL APIs are not implemented.

<br/>

## Virtual-Hosted Addressing

Path-style requests (`http://s3.example.com/<bucket>/<key>`) always work. With `PLAINBUCKET_DOMAINS=s3.example.com`, a request whose `Host` is `<bucket>.s3.example.com` is also accepted: the first label names the bucket and the whole path is the key. Point a wildcard DNS record, and a wildcard certificate for HTTPS, at the server. When several domains match, the longest wins. A host that is the domain itself, or whose first label is not a valid bucket name, is served path-style.

Use a domain dedicated to plainbucket: with `example.com` configured, `www.example.com` would name a bucket `www`. On the bucket hosts, `/healthz` and `/metrics` are object keys like any other.

<br/>

## Slow Uploads

The server bounds how long it waits for request headers, but an object body can take as long as it needs, so large uploads over slow links work. What it does not accept is an upload that stops making progress: each window of `PLAINBUCKET_BODY_TIMEOUT` must deliver at least 32 KiB, or the connection is closed. This keeps a client that trickles a byte now and then from holding a connection and a staging file forever.

<br/>

## Metrics

With `PLAINBUCKET_METRICS=true`, `GET /metrics` returns Prometheus text format without authentication:

- `plainbucket_requests_total{method,status}` and the `plainbucket_request_duration_seconds` histogram.
- `plainbucket_received_bytes_total` (stored by successful uploads) and `plainbucket_sent_bytes_total`.
- `plainbucket_objects`, `plainbucket_stored_bytes` and `plainbucket_usage_scan_timestamp_seconds`, measured at startup and then hourly by walking the buckets, so they lag recent uploads.

A bucket named `metrics` or `healthz` stays reachable: S3 requests are signed, and only unsigned `GET` requests are answered by these endpoints.

<br/>

## Background Cleanup

Once an hour the server removes staging files left by interrupted uploads, multipart uploads older than `PLAINBUCKET_UPLOAD_EXPIRY`, and metadata whose object was deleted outside the server.
