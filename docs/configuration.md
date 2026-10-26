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
| `PLAINBUCKET_PUBLIC_BUCKETS` | `public-buckets` | | Comma-separated buckets whose objects anyone may read; see [Buckets](#buckets) |
| | `buckets` | | Per-bucket settings: public reads, static website, CORS, expiry, size limit; see [Buckets](#buckets) |
| `PLAINBUCKET_DOMAINS` | `domains` | | Comma-separated domains for virtual-hosted-style requests; see [Virtual-Hosted Addressing](#virtual-hosted-addressing) |
| `PLAINBUCKET_REGION` | `region` | `us-east-1` | Region reported by `GetBucketLocation` and `HeadBucket` |
| `PLAINBUCKET_MAX_OBJECT_SIZE` | `max-object-size` | `0` | Largest object or part in bytes; `0` means no limit |
| `PLAINBUCKET_MIN_FREE_SPACE` | `min-free-space` | `0` | Free bytes every write must leave on the root's filesystem; `0` turns the check off. See [Free Space](#free-space) |
| `PLAINBUCKET_UPLOAD_EXPIRY` | `upload-expiry` | `168h` | Age after which an unfinished multipart upload is removed |
| `PLAINBUCKET_BODY_TIMEOUT` | `body-timeout` | `1m` | Cut an upload that delivers less than 32 KiB in this long; `0` disables it |
| `PLAINBUCKET_METRICS` | `metrics` | `false` | Serve Prometheus metrics at `GET /metrics` |
| `PLAINBUCKET_CONSOLE` | `console` | `false` | Serve the web console at `/_console/`; see [Web Console](#web-console) |
| `PLAINBUCKET_LOG_FORMAT` | `log-format` | `text` | `text` or `json` |
| `PLAINBUCKET_DEBUG` | `debug` | `false` | Log the startup configuration, denied requests and, like `PLAINBUCKET_ACCESS_LOG`, every request |
| `PLAINBUCKET_ACCESS_LOG` | `access-log` | `false` | Log every request at info level; see [Access Log](#access-log) |
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

## Reloading

A running server re-reads its configuration on `SIGHUP` (`docker kill --signal=HUP`, `systemctl reload` with `ExecReload=kill -HUP $MAINPID`), and on its own when the config file, the credentials file, a webhook's `secret-file` or the TLS certificate or key changes; it checks them every 30 seconds.

- Taken over without a restart: the access keys (`access-key`, `secret-key`, `credentials`, `credentials-file`), the `buckets` map and `public-buckets`, `min-free-space`, and the certificate files. A request already authenticated finishes with the key it was signed with.
- Every other setting changes only on restart; the server logs which ones differ. Environment variables are read again too, but a process's environment does not change after it starts, so values set there need a restart.
- A file that fails to load or validate leaves the running configuration as it was, and the error is logged.

In Kubernetes, mount the credentials Secret and the TLS Secret as directories, not with `subPath`, so updates reach the files; the [Helm chart](../helm/plainbucket/README.md) does.

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

## Buckets

Buckets need no configuration: any valid directory under the root is a bucket. The `buckets` map in the YAML file changes how one behaves:

```yaml
buckets:
  www:
    public: true
    website:
      index: index.html     # the default
      error: 404.html
    cors:
      allowed-origins: ["https://app.example.com", "https://*.preview.example.com"]
      allowed-methods: [GET, HEAD]   # the default
      allowed-headers: ["*"]
      expose-headers: [ETag]
      max-age: 1h
  runner-cache:
    expire-after: 336h      # remove objects unused for two weeks
    max-size: 85899345920   # bytes
    evict: true             # over max-size, remove the least recently used
  docs:
    trash-after: 168h       # deleted objects stay restorable for a week
  uploads:
    notify:                 # POST the bucket's events to a webhook
      - url: https://hooks.example.com/plainbucket
        events: ["s3:ObjectCreated:*"]
        prefix: incoming/
        secret-file: /etc/plainbucket/notify/key
```

These settings are the only way to change bucket behavior: the bucket policy, ACL, website and CORS APIs are not implemented.

### Public Reads

With `public: true`, or the bucket named in `PLAINBUCKET_PUBLIC_BUCKETS`, anyone can fetch the bucket's objects with an unsigned `GET` or `HEAD`, as with a bucket policy granting `s3:GetObject` to everyone. Listing the bucket, multipart state, tags and every write still need a key, and an unsigned request may not use the `response-*` overrides, as on S3. Signed requests are unaffected.

### Static Website

`website` turns a public bucket into a static site for unsigned requests, as an S3 website endpoint does:

- The bucket root and every path ending in `/` serve that prefix's index document.
- A path without the trailing `/` whose prefix has an index document is redirected to the path with it (`/docs` to `/docs/`).
- A missing key serves the error document with status `404`, or S3's XML error when there is none.

Signed requests keep S3 semantics, so clients and the aws cli still list the bucket and get `NoSuchKey`. Combined with [virtual-hosted addressing](#virtual-hosted-addressing), `www.s3.example.com/` serves the site from its own host name.

### CORS

`cors` lets pages on other origins use the bucket from a browser. An allowed origin gets `Access-Control-Allow-Origin` (and `Access-Control-Expose-Headers`) on every response, errors included, and every response from the bucket carries `Vary: Origin` so caches keep origins apart. A preflight `OPTIONS` request is answered without a signature: `200` when the origin, the method and every requested header are allowed, `403` otherwise. Origins and headers may hold one `*` wildcard; header names compare case-insensitively. A browser that uploads with signed requests needs `PUT` in `allowed-methods` and the signing headers (`Authorization`, `X-Amz-*`, `Content-Type`) in `allowed-headers`.

### Expiry and Size Limits

An object's last use is the later of its last write and its last `GET` or `HEAD`. A read records itself by moving the modification time of the object's metadata file, at most once an hour, or once a quarter of `expire-after` when that is shorter; the data file is never touched.

- `expire-after` removes objects not used for that long. It suits caches: the GitLab Runner does not re-upload an unchanged cache, so expiring by upload time would remove caches in daily use.
- `max-size` caps the bucket's bytes. By default a write that would pass it is refused with `403 QuotaExceeded`. With `evict: true` the write is accepted and the least recently used objects are removed until the bucket fits again; only an object larger than the whole limit is refused.

The background job applies both hourly, and at once when a write takes an evicting bucket over its limit, so expiry is as precise as an hour. The server measures the buckets before it starts serving, then tracks usage as objects are written and deleted and re-measures hourly to include files changed outside the server; a file changed outside the server counts only from the next measurement. An object rewritten or read between the scan and its removal is kept. Incomplete multipart uploads do not count until they are completed, and files whose names cannot be keys are not counted against a limit.


### Trash

`trash-after` keeps the objects deleted from the bucket (`DeleteObject`, `DeleteObjects`, and the web console's delete, rename and move) restorable for that long. A deleted object leaves the bucket at once, so listings, reads and the bucket's usage and limits no longer see it, but its file and metadata move to the system directory instead of being removed. The web console lists a bucket's trash and restores or removes each entry; a restore never replaces an object that has since been written at the same key. Once the period ends the hourly background job removes the entry for good, as does `plainbucket fsck --repair`.

Only deletes are kept: an overwrite replaces the object, and expiry and eviction under the bucket's limits remove objects for good. Trashed objects still take disk space until they are removed. Turning `trash-after` off keeps the entries already trashed until their own period ends.

### Event Notifications

`notify` lists webhooks that receive the bucket's object events as an HTTP `POST` of an [S3 event notification](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-content-structure.html) document, one record per request, so code written for S3 events reads them. Each entry takes:

- `url`: an `http` or `https` URL. A redirect counts as a failure. The URL sits in the config file and is logged without its query, so put credentials in the signing key, not in the URL.
- `events`: S3 event names, or a group ending in `*`; empty means all of them. `s3:ObjectCreated:Put`, `Post`, `Copy` and `CompleteMultipartUpload`; `s3:ObjectRemoved:Delete`, sent only when a delete removed an object (moving it to the [trash](#trash) counts); and `s3:LifecycleExpiration:Delete` for objects that expiry or eviction removed. Restoring from the trash is an `s3:ObjectCreated:Put`.
- `prefix`, `suffix`: only keys that start or end with these.
- `id`: the record's `configurationId`, `notify-<n>` by default.
- `secret-file`: a file holding the key that signs each request; a trailing newline is not part of it. Without one the requests are unsigned.

A signed request carries `X-Plainbucket-Timestamp` (Unix seconds) and `X-Plainbucket-Signature: sha256=<hex>`, the HMAC-SHA256 of the timestamp, a `.` and the body. A receiver recomputes it over the raw body, compares in constant time, and may refuse timestamps more than a few minutes old to stop replays; each retry is signed again with its own timestamp. `X-Plainbucket-Event` names the event. The record's object `key` is form-encoded as in S3 events (a space becomes `+`), `size` and `eTag` are set for created objects, and `sequencer` grows from event to event when compared as strings, though two requests changing one key at the same moment may be numbered in the order their events were sent rather than stored.

Delivery is best-effort. Each webhook has a queue in memory that holds a thousand events; when it is full new events are dropped. A failed request is retried four times over about fifteen seconds when the connection fails or the answer is `429` or `5xx`, and given up on for any other status. Events still queued when the server stops get a few seconds to go out, and are lost after that or on a crash. Drops and failures are logged as warnings and counted in the [metrics](#metrics). The notification API itself is not implemented: webhooks are configured here, and a reload takes changes to them, including a rotated `secret-file`, over without a restart.

<br/>

## Virtual-Hosted Addressing

Path-style requests (`http://s3.example.com/<bucket>/<key>`) always work. With `PLAINBUCKET_DOMAINS=s3.example.com`, a request whose `Host` is `<bucket>.s3.example.com` is also accepted: the first label names the bucket and the whole path is the key. Point a wildcard DNS record, and a wildcard certificate for HTTPS, at the server. When several domains match, the longest wins. A host that is the domain itself, or whose first label is not a valid bucket name, is served path-style.

Use a domain dedicated to plainbucket: with `example.com` configured, `www.example.com` would name a bucket `www`. On the bucket hosts, `/healthz` and `/metrics` are object keys like any other.

<br/>

## Slow Uploads

The server bounds how long it waits for request headers, but an object body can take as long as it needs, so large uploads over slow links work. What it does not accept is an upload that stops making progress: each window of `PLAINBUCKET_BODY_TIMEOUT` must deliver at least 32 KiB, or the connection is closed. This keeps a client that trickles a byte now and then from holding a connection and a staging file forever.

<br/>

## Free Space

With `PLAINBUCKET_MIN_FREE_SPACE` set, a write that would leave less than that many bytes free on the filesystem holding the root is refused with `507 InsufficientStorage` before its body is stored. Every write counts: objects, parts, copies and POST uploads, and completing a multipart upload, which writes the object's size once more while it joins the parts. Writes in progress hold their size against each other, so parallel uploads cannot pass the minimum together. A body sent without a length is cut off with the same error once it would. Reads and deletes go on, so a full server can still be emptied.

A filesystem that fills up anyway (another writer, a quota, or no minimum set) gives the same `507` instead of an internal error. Every `507` is logged as a warning. On platforms where free space cannot be read, the minimum is not enforced and the server logs a warning at startup.

<br/>

## Metrics

With `PLAINBUCKET_METRICS=true`, `GET /metrics` returns Prometheus text format without authentication:

- `plainbucket_requests_total{operation,method,status}` and the `plainbucket_request_duration_seconds{operation}` histogram. `operation` is the S3 operation name (`PutObject`, `UploadPart`, `ListObjectsV2`, ...), decided from the request alone, so refused requests count under the operation they asked for; `Console` covers the web console, `Preflight` CORS preflights, `NotImplemented` unsupported operations and `Unknown` anything else.
- `plainbucket_received_bytes_total` (stored by successful uploads) and `plainbucket_sent_bytes_total`.
- `plainbucket_objects`, `plainbucket_stored_bytes` and `plainbucket_usage_scan_timestamp_seconds`, measured at startup and then hourly by walking the buckets, so they lag recent uploads.
- `plainbucket_filesystem_free_bytes` and `plainbucket_filesystem_size_bytes` for the filesystem holding the root, read at each scrape, and `plainbucket_min_free_bytes`, the configured minimum.
- `plainbucket_notifications_total{result}` for [event notifications](#event-notifications), `sent`, `failed` (given up after retries, or refused) and `dropped` (a full queue, or still queued at shutdown), and `plainbucket_notification_queue`, the events waiting.
- For buckets with `expire-after` or `max-size`: `plainbucket_bucket_objects{bucket}` and `plainbucket_bucket_bytes{bucket}`, kept current between scans, `plainbucket_bucket_max_size_bytes{bucket,evict}` for those with `max-size`, and the counters `plainbucket_expired_objects_total` and `plainbucket_evicted_objects_total`.

A bucket named `metrics` or `healthz` stays reachable: S3 requests are signed, and only unsigned `GET` requests are answered by these endpoints.

<br/>

## Web Console

With `PLAINBUCKET_CONSOLE=true`, `/_console/` serves a web console: browse buckets and folders with each bucket's settings and usage, filter and sort a folder, search a bucket by name, preview images, video, audio and the start of text files, upload files and folders (large files as multipart uploads, with retry), download, share an object through a presigned URL that lasts up to seven days or as a `curl` command, rename or move objects and folders, edit an object's metadata, delete objects, folders and empty buckets, and abort multipart uploads left behind, and restore objects from a bucket's [trash](#trash). A read-only key sees no write actions.

- Sign in with an access key and its secret key. The page signs every request itself and sends it to the S3 API, so the key's bucket scope and read-only flag apply as they do to any client. The secret key never leaves the page; it is kept in memory only, so reloading the page signs out.
- The pages follow the system's light or dark theme; the Theme button forces either one and is remembered in the browser's local storage. Nothing else, and never the key, is stored there.
- Uploads never replace an object silently: they are sent with `If-None-Match: *`, and a name that already exists is offered for overwriting or skipping.
- After 30 minutes without input the page signs out and forgets the key, unless an upload is still running.
- Renaming or moving copies each object with `If-None-Match: *`, so nothing at the new name is overwritten, and deletes the originals only after every copy succeeded. It is not atomic: another client can see both names while a folder moves. Editing metadata copies the object onto itself, refused if it changed since the dialog opened.
- The console works over plain HTTP too: it does not rely on the browser's Web Crypto API, which only secure contexts offer. Over plain HTTP, though, anyone on the network path can read the secret key as it is typed or replace the page's script, so serve the console over TLS anywhere but a trusted network.
- The pages are served without authentication; they hold no data. Two APIs of the console's own need a signed request. `GET /_console/api/usage` lists only the buckets the key may access, with the total across buckets and the disk's free space only for a key without a bucket scope; writes and deletes through the server update it at once, and files changed outside the server count from the next hourly measurement. `/_console/api/trash` lists, restores and removes a bucket's [trash](#trash) within the key's bucket scope and read-only flag.
- The console answers only on path-style hosts. On a virtual host, `/_console/` is an object key of that bucket.
- Every object the console's host serves (public and website buckets, presigned and shared links, signed reads) would otherwise share the console's origin, so a stored page could script a console window in the same browser. With the console on, those responses carry `Content-Security-Policy: sandbox allow-scripts allow-popups allow-forms`: the page still renders and runs its scripts, but in an opaque origin, so it cannot reach the console, its own server through `fetch`, cookies or local storage. Serve a website bucket that needs any of those through [virtual-hosted addressing](#virtual-hosted-addressing), which gives it its own origin and no sandbox.

<br/>

## Background Cleanup

Once an hour the server removes staging files left by interrupted uploads, multipart uploads older than `PLAINBUCKET_UPLOAD_EXPIRY`, and metadata whose object was deleted outside the server, and applies the [bucket limits](#expiry-and-size-limits).

<br/>

## Access Log

With `PLAINBUCKET_ACCESS_LOG=true` every API request is logged at info level, in the format `PLAINBUCKET_LOG_FORMAT` selects, with `operation` (as in the metrics), `method`, `host`, `path`, `query`, `bucket`, `key`, `status`, `bytes` (sent), `duration_ms`, `remote`, `access_key` (empty for unsigned requests), `user_agent` and `request_id`, which is also the `x-amz-request-id` response header. The signature of a presigned URL is logged as `REDACTED`, so the log cannot be used to replay requests. `/healthz` and `/metrics` are not logged.

<br/>

## Commands

- `plainbucket` (no command) serves.
- `plainbucket healthcheck` exits 0 when the server on this host answers `/healthz`. It reads the same settings to find the address and whether TLS is on, but needs no access key, so the container image uses it as its `HEALTHCHECK`. A server started with `-c` needs the same `-c` here.
- `plainbucket fsck [--repair]` checks the root while no server runs: objects whose metadata is missing or outdated, file names that cannot be keys, and the leftovers the background cleanup removes. It exits 1 when problems remain. `--repair` rebuilds the metadata (the object then counts as just used) and removes the leftovers; invalid names are only reported. The server holds a lock on the root, so fsck refuses to run beside it, and a second server on the same root refuses to start.
- `plainbucket version` prints the version.
