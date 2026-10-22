# S3 Compatibility

plainbucket implements the part of the S3 REST API that general-purpose clients use to store and fetch objects. The AWS SDK for Go v2 (`compat/`) and the aws cli (`hack/test-awscli.sh`) run against it in CI.

<br/>

## Addressing and Authentication

- Path-style URLs only: `http://host:9000/<bucket>/<key>`. Configure SDKs with path-style addressing (`UsePathStyle` in Go, `addressing_style = path` in boto3 and the aws cli; the aws cli uses path style on its own for a custom endpoint).
- AWS Signature Version 4 in the `Authorization` header or as a presigned URL, up to seven days.
- Payload modes: a hex SHA-256 of the body, `UNSIGNED-PAYLOAD`, and aws-chunked bodies `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER`. Chunk and trailer signatures are verified.
- Every `x-amz-*` header in a request must be covered by its signature, as on S3. An unsigned one is refused with `403 AccessDenied`, so a presigned URL cannot be repurposed with extra headers such as `x-amz-copy-source`.
- Signature Version 2 and anonymous requests are refused.

<br/>

## Supported Operations

| Area | Operations |
|---|---|
| Service | `ListBuckets` |
| Bucket | `CreateBucket`, `HeadBucket`, `DeleteBucket` (must be empty), `GetBucketLocation`, `GetBucketVersioning` (always unversioned) |
| Listing | `ListObjectsV2` and `ListObjects` with `prefix`, `delimiter`, `max-keys`, `start-after`, `continuation-token` / `marker`, `fetch-owner`, `encoding-type=url` |
| Object | `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject` (`COPY` or `REPLACE` metadata directive) |
| Multipart | `CreateMultipartUpload`, `UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListParts`, `ListMultipartUploads` |

Object details:

- `GetObject` supports `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and the `response-*` overrides presigned URLs use.
- Writes accept `If-None-Match: *` (create only if absent, as the Terraform S3 backend lock uses) and `If-Match: <etag>`. `CopyObject` also accepts `x-amz-copy-source-if-match` and `-if-none-match`.
- Stored with the object and returned on read: `Content-Type`, `x-amz-meta-*`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`.
- `Content-MD5`, `x-amz-checksum-*` headers and trailing checksums are verified. Full-object checksums come back on `GetObject` / `HeadObject` with `x-amz-checksum-mode: ENABLED` when the whole object is read.
- ETags follow S3: the MD5 of the object, or for a multipart upload the MD5 of the part MD5s followed by `-<parts>`.

<br/>

## Not Implemented

These return `501 NotImplemented`: ACLs and policies, versioning other than reading its status, object lock and retention, tagging, lifecycle, replication, CORS and website configuration, encryption settings, `UploadPartCopy`, `GetObject` by part number, `SelectObjectContent`, and delimiters in `ListMultipartUploads`. Virtual-hosted-style addressing is not supported.

Storage classes are not modeled; every object reports `STANDARD`.

<br/>

## Error Codes Specific to plainbucket

| Code | Status | Cause |
|---|---|---|
| `KeyConflict` | 409 | The key needs a path whose parent is an existing object, or names an existing directory. See [layout.md](layout.md). |
| `InvalidArgument` | 400 | The key cannot be a relative file path, or a key ending in `/` has a body. |
