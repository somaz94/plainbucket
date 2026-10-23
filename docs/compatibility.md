# S3 Compatibility

plainbucket implements the part of the S3 REST API that general-purpose clients use to store and fetch objects. CI runs these clients against it:

- the AWS SDK for Go v2, including the multipart upload and download managers (`compat/`);
- the aws cli (`hack/test-awscli.sh`);
- rclone, including `check` by hash, `sync` with deletion, server-side copy and move, and directory markers (`hack/test-rclone.sh`);
- the official `gitlab-runner` binary's cache helper, with presigned URLs and with Go Cloud URLs (`hack/test-gitlab-runner.sh`, see [gitlab-runner.md](gitlab-runner.md)). Older runners send `HEAD` on the URL presigned for `GET`; that is accepted for them, though S3 itself would refuse it, and `compat/` replays that flow.
- a subset of [ceph/s3-tests](#ceph-s3-tests), the conformance suite other S3 servers run, for error codes and edge cases the clients above never exercise.

<br/>

## Addressing and Authentication

- Path-style URLs (`http://host:9000/<bucket>/<key>`) always work. Configure SDKs with path-style addressing (`UsePathStyle` in Go, `addressing_style = path` in boto3 and the aws cli; the aws cli uses path style on its own for a custom endpoint).
- Virtual-hosted-style URLs (`http://<bucket>.s3.example.com/<key>`) work for the domains listed in `PLAINBUCKET_DOMAINS` ([Virtual-Hosted Addressing](configuration.md#virtual-hosted-addressing)).
- AWS Signature Version 4 in the `Authorization` header or as a presigned URL, up to seven days.
- Payload modes: a hex SHA-256 of the body, `UNSIGNED-PAYLOAD`, and aws-chunked bodies `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER`. Chunk and trailer signatures are verified.
- Every `x-amz-*` header in a request must be covered by its signature, as on S3. An unsigned one is refused with `403 AccessDenied`, so a presigned URL cannot be repurposed with extra headers such as `x-amz-copy-source`.
- Signature Version 2 is refused. Anonymous requests are refused, except `GET` and `HEAD` of objects in buckets configured as public ([Buckets](configuration.md#buckets)).
- With the [web console](configuration.md#web-console) on, `/_console/` on a path-style host is the console rather than S3. No bucket can be named `_console`, so no bucket is hidden by it. Objects served on that host then carry a sandboxing `Content-Security-Policy`.

<br/>

## Supported Operations

| Area | Operations |
|---|---|
| Service | `ListBuckets` |
| Bucket | `CreateBucket`, `HeadBucket`, `DeleteBucket` (must be empty), `GetBucketLocation`, `GetBucketVersioning` (always unversioned) |
| Listing | `ListObjectsV2` and `ListObjects` with `prefix`, `delimiter`, `max-keys`, `start-after`, `continuation-token` / `marker`, `fetch-owner`, `encoding-type=url`; `ListObjectVersions`, which lists every object as its only version, `null`, as S3 does for an unversioned bucket |
| Object | `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject` (`COPY` or `REPLACE` metadata directive), `GetObjectAttributes`, `GetObjectTagging` (always an empty tag set) |
| Multipart | `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy` (with `x-amz-copy-source-range`; a range outside the source is `400 InvalidArgument`, as on S3), `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListParts`, `ListMultipartUploads` |

Object details:

- `GetObject` supports `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and the `response-*` overrides presigned URLs use.
- `GetObject` and `HeadObject` with `partNumber` serve that part of a multipart object as `206` with `x-amz-mp-parts-count`; an object written in one piece is part 1. A part beyond the last is `416 InvalidPartNumber`, as on S3.
- Writes accept `If-None-Match: *` (create only if absent, as the Terraform S3 backend lock uses), `If-None-Match: <etag>` and `If-Match: <etag>`. `CopyObject` and `UploadPartCopy` also accept `x-amz-copy-source-if-match` and `-if-none-match`.
- Stored with the object and returned on read: `Content-Type`, `x-amz-meta-*`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`.
- `Content-MD5`, `x-amz-checksum-*` headers and trailing checksums are verified. Checksums come back on `GetObject` / `HeadObject` with `x-amz-checksum-mode: ENABLED` when the whole object is read.
- A multipart upload created with `x-amz-checksum-algorithm` gets an object checksum of the type S3 allows for it (`x-amz-checksum-type`): `COMPOSITE`, the digest of the part digests followed by `-<parts>`, or `FULL_OBJECT` for the CRCs, computed over the assembled data. `CompleteMultipartUpload` checks the checksum it is given against it, and the part checksums in its document against the parts. A copy keeps a full-object checksum and drops a composite one.
- ETags follow S3: the MD5 of the object, or for a multipart upload the MD5 of the part MD5s followed by `-<parts>`.

<br/>

## Not Implemented

These return `501 NotImplemented`: ACLs and policies (public reads are configured instead), versioning other than reading its status and listing the `null` versions, object lock and retention, storing tags, lifecycle, replication, the CORS and website APIs (both are configured per bucket instead, see [Buckets](configuration.md#buckets)), encryption settings, browser `POST` uploads, `SelectObjectContent`, and delimiters in `ListMultipartUploads`.

Tags are not stored. `GetObjectTagging` answers with an empty set because the aws cli and other tools read the tags of an object before copying it, and fail on `501`; `PutObjectTagging` and `DeleteObjectTagging` stay unimplemented.

Storage classes are not modeled; every object reports `STANDARD`.

<br/>

## ceph/s3-tests

`make test-s3tests` runs [ceph/s3-tests](https://github.com/ceph/s3-tests) against `bin/plainbucket` at the commit pinned in `hack/test-s3tests.sh`, with the Python packages frozen in `hack/s3tests-requirements.txt`. The standalone repository was archived when the suite moved into the Ceph source tree, so the pinned commit does not move.

- Tests of features listed under [Not Implemented](#not-implemented) are deselected by their pytest markers (the list is in the script). Tests that assume several owners, ACLs or RGW-specific behavior run but stay off the pass list.
- `hack/s3tests-pass.txt` lists the tests that pass. A listed test that fails breaks the run; a test that starts passing is reported, and `make s3tests-update` rewrites the list from a run. The JUnit report is kept in `bin/s3tests.xml` and uploaded by CI.
- Where s3-tests expects RGW's answer and S3 gives another, plainbucket follows S3, and the test stays off the list: a `partNumber` beyond the last part is `416 InvalidPartNumber`, not RGW's `400 InvalidPart`.

<br/>

## Error Codes Specific to plainbucket

| Code | Status | Cause |
|---|---|---|
| `KeyConflict` | 409 | The key needs a path whose parent is an existing object, or names an existing directory. See [layout.md](layout.md). |
| `InvalidArgument` | 400 | The key cannot be a relative file path, or a key ending in `/` has a body. |
