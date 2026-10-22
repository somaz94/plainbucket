# Configuration

Each setting can be given as an environment variable or in a YAML file passed with `-c` / `--config`. An environment variable wins over the file, and the file wins over the default. Unknown YAML keys and malformed typed values (`PLAINBUCKET_UPLOAD_EXPIRY=7d`) are startup errors, not silently ignored.

<br/>

## Settings

| Environment variable | YAML key | Default | Meaning |
|---|---|---|---|
| `PLAINBUCKET_ROOT` | `root` | `/data` | Directory holding the buckets |
| `PLAINBUCKET_ADDRESS` | `address` | `:9000` | Listen address, `host:port` |
| `PLAINBUCKET_ACCESS_KEY` | `access-key` | | Access key clients sign with (required) |
| `PLAINBUCKET_SECRET_KEY` | `secret-key` | | Secret key (required) |
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

The server has one access key. Every request must be signed with AWS Signature Version 4, in the `Authorization` header or as a presigned URL. Unsigned requests get `403 AccessDenied`; Signature Version 2 is refused. Only `/healthz`, and `/metrics` when enabled, answer without a signature.

The region in a request's credential scope is not checked against `PLAINBUCKET_REGION`, so clients configured for any region work.

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
