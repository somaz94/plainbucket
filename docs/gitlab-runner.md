# GitLab Runner Distributed Cache

plainbucket can hold the distributed cache of GitLab Runner. Cache archives end up as plain zip files under `<root>/<bucket>/`, so they can be inspected or pruned with ordinary tools.

<br/>

## Runner Configuration

Give the runner its own key, limited to the cache bucket ([Credentials](configuration.md#credentials)):

```yaml
credentials:
  - access-key: gitlab-runner
    secret-key: change-me
    buckets: [runner-cache]
```

Create the bucket once with any S3 client, then point the runner at it in `config.toml`:

```toml
[runners.cache]
  Type = "s3"
  Shared = true
  [runners.cache.s3]
    ServerAddress = "plainbucket.example.com:9000"
    AccessKey = "gitlab-runner"
    SecretKey = "change-me"
    BucketName = "runner-cache"
    BucketLocation = "us-east-1"
    PathStyle = true
    Insecure = true   # plain HTTP; remove when plainbucket serves HTTPS
```

Without `PathStyle = true`, the runner addresses the bucket as `runner-cache.<ServerAddress>`, which works only when that domain is configured in `PLAINBUCKET_DOMAINS` and resolves to the server.

<br/>

## What Is Tested

`make test-runner` (`hack/test-gitlab-runner.sh`, also run in CI) downloads the official `gitlab-runner` binary at a pinned version, verifies its SHA-256, and runs its cache helper commands against plainbucket:

- `cache-archiver` uploading with a presigned PUT URL, and skipping the upload when a presigned HEAD URL (`--check-url`) finds the cache already there;
- `cache-extractor` downloading with a presigned GET URL, with and without the presigned HEAD check (`--head-url`);
- both commands with a Go Cloud `s3://` URL, which sign each request with the credentials instead of using presigned URLs.

The presigned URLs are made by the AWS SDK for Go v2, as the runner manager makes them.

Older runners probe a cache with `HEAD` on the URL presigned for `GET`. S3 refuses that, but plainbucket accepts it, and `compat/` replays that flow.
