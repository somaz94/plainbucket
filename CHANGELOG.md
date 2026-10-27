# Changelog

All notable changes to this project will be documented in this file.

## [v0.8.0](https://github.com/somaz94/plainbucket/compare/v0.7.0...v0.8.0) (2026-10-27)

### Features

- replicate buckets to another S3 endpoint ([42a05fb](https://github.com/somaz94/plainbucket/commit/42a05fbc9d7da383d120c5a542fbfb935543837e))
- outgoing SigV4 signer and an S3 client ([4054259](https://github.com/somaz94/plainbucket/commit/4054259adfb6a27460f6e1d51f4494a1cad57838))
- bucket tagging API ([bea0b9e](https://github.com/somaz94/plainbucket/commit/bea0b9e87428c7ac91a4c7b66d6e048387332dfb))
- inventory command listing every object as CSV or JSON Lines ([3507461](https://github.com/somaz94/plainbucket/commit/35074619e6d67547062a41c57ce031bae7024da1))
- bucket-metrics reporting every bucket's usage ([dbb86d6](https://github.com/somaz94/plainbucket/commit/dbb86d683e8b989a7b50d459b8c64269dfcef7d4))
- /readyz probing the storage root, used by the chart's readiness probe ([c501939](https://github.com/somaz94/plainbucket/commit/c5019396c7b0e9f1a4433e2f57962d02fcd55449))
- STS GetCallerIdentity for long-term and temporary keys ([dd98766](https://github.com/somaz94/plainbucket/commit/dd9876614a4983f4b0aa26f01f0be44d0457a9f4))

### Bug Fixes

- keep the tags CreateBucket sends ([a915437](https://github.com/somaz94/plainbucket/commit/a915437ee7d7c1e26b958b02b4c3ca56907048b9))
- keep bucket usage exact when buckets are created or deleted during a measurement ([bb37473](https://github.com/somaz94/plainbucket/commit/bb37473440c599390e9a145c1d7c28521aa3844f))

### Tests

- terraform s3 backend end-to-end with its lock file ([70ad3b0](https://github.com/somaz94/plainbucket/commit/70ad3b0fcf348f6f1c36b4b13af064b98e36fa28))

### Chores

- bump version to v0.8.0 ([cca8fc4](https://github.com/somaz94/plainbucket/commit/cca8fc463a27fd154d49e32a212d8061059eefab))

### Contributors

- somaz

<br/>

## [v0.7.0](https://github.com/somaz94/plainbucket/compare/v0.6.1...v0.7.0) (2026-10-27)

### Features

- temporary credentials through STS AssumeRole and the console ([027d07b](https://github.com/somaz94/plainbucket/commit/027d07b2fc0130f2b1133ee4b47148d16e7a6e2c))
- console upload links that need no key ([ca17bed](https://github.com/somaz94/plainbucket/commit/ca17beda8de7e9c0e34147ac0afc4237cb949022))
- per-key and per-address rate limits for requests and bandwidth ([b32d4bc](https://github.com/somaz94/plainbucket/commit/b32d4bc3f75db0172ea5cf83c25ceb3d957e5847))
- per-prefix lifecycle rules expiring by last use or write time ([deecaa5](https://github.com/somaz94/plainbucket/commit/deecaa5063dd03d2c19f8deb2ca5c9469fd6bd76))
- integrity checks that re-read object data against its recorded digests ([81441ac](https://github.com/somaz94/plainbucket/commit/81441acca54400ab462567e35d8528b418e0578f))
- per-bucket default response headers by key prefix and suffix ([bee6fd8](https://github.com/somaz94/plainbucket/commit/bee6fd8109f299922e31c17b5d50ac9c77fd115a))
- server-wide read-only mode for maintenance ([a40cb3d](https://github.com/somaz94/plainbucket/commit/a40cb3d761855aa3a52964cc6480fa5437d137fc))

### Performance Improvements

- stat only the entries a listing reaches ([9c43d15](https://github.com/somaz94/plainbucket/commit/9c43d156e983a178c1203d361c0a82ec0f94915d))

### Tests

- match the header schema error by header name ([1f3611b](https://github.com/somaz94/plainbucket/commit/1f3611bb9e6fb51ed238772bbfcdfb6b82e59dc4))

### Chores

- bump version to v0.7.0 ([9ab7b07](https://github.com/somaz94/plainbucket/commit/9ab7b072fc081013d0fc6b563443f8cb3b625285))

### Contributors

- somaz

<br/>

## [v0.6.1](https://github.com/somaz94/plainbucket/compare/v0.6.0...v0.6.1) (2026-10-26)

### Features

- chart alert and dashboard panels for failing event notifications ([079f366](https://github.com/somaz94/plainbucket/commit/079f366cdccace1681747eb3e56e229b011c51cc))

### Chores

- bump version to v0.6.1 ([9045c20](https://github.com/somaz94/plainbucket/commit/9045c2040d547a2f517cee6ed8711c85ba3140e1))

### Contributors

- somaz

<br/>

## [v0.6.0](https://github.com/somaz94/plainbucket/compare/v0.5.0...v0.6.0) (2026-10-26)

### Features

- download folders as ZIP files, size folders and add keyboard shortcuts to the console ([5134434](https://github.com/somaz94/plainbucket/commit/513443482f65da9720d60a97279ca69f330c9598))
- post bucket events to signed webhooks ([53d7f50](https://github.com/somaz94/plainbucket/commit/53d7f502a092c030ad20d41dd094ef4f8993c4ae))
- keep deleted objects in a per-bucket trash restorable from the console ([216b12c](https://github.com/somaz94/plainbucket/commit/216b12ca40984d668c61d39f573774ac00dbe514))
- chart PrometheusRule alerts and a Grafana dashboard ([029eded](https://github.com/somaz94/plainbucket/commit/029ededb9f678a86d1c871319b7dfa1046b5f568))
- store object tags with put, get and delete tagging ([ce6ea5e](https://github.com/somaz94/plainbucket/commit/ce6ea5ee208fc0922f2060822e749a1c67c05c40))
- conditional deletes and delimiters in ListMultipartUploads ([cd0edda](https://github.com/somaz94/plainbucket/commit/cd0eddabad54877c24f001ccba102b7c5c0f9d2f))
- refuse writes that would pass a free disk space floor ([a49cbaf](https://github.com/somaz94/plainbucket/commit/a49cbaf6cf90e93e5e7265c826139a7c1b42a7a0))

### Bug Fixes

- stop the console's bucket summary from showing null ([4ce14a1](https://github.com/somaz94/plainbucket/commit/4ce14a10dfc817c64bc9c6841364d1db2dc587d6))
- keep the object d when deleting the folder marker d/ ([42354e1](https://github.com/somaz94/plainbucket/commit/42354e1b1cbfa2ac0c1ce2024bf4ef058b52ff95))

### Tests

- match chart schema errors by key name across Helm versions ([37ead67](https://github.com/somaz94/plainbucket/commit/37ead67606730a342a02be69d0ce4dafc53f4962))

### Chores

- bump version to v0.6.0 ([df24181](https://github.com/somaz94/plainbucket/commit/df2418155a1c18826f7c59de1fe5d8b967c4d03d))

### Contributors

- somaz

<br/>

## [v0.5.0](https://github.com/somaz94/plainbucket/compare/v0.4.0...v0.5.0) (2026-10-23)

### Features

- accept browser POST uploads signed with a SigV4 policy ([39ec016](https://github.com/somaz94/plainbucket/commit/39ec0169d7bd5271e8849269092243bbb684da43))
- reload keys, bucket settings and the TLS certificate live ([f5316d8](https://github.com/somaz94/plainbucket/commit/f5316d8d6e1ff512c08adc261e31636da9c0619a))
- label request metrics and the access log with the S3 operation ([f79fb6e](https://github.com/somaz94/plainbucket/commit/f79fb6ef50ab1a64c4a64b75e68391f67e9316ac))
- page ListBuckets and accept chunked PutObject bodies ([449d716](https://github.com/somaz94/plainbucket/commit/449d7168cce96eaf60eb2323386fe30b79727709))
- add console overwrite guard, idle sign-out, search and attributes ([5993250](https://github.com/somaz94/plainbucket/commit/59932507c7fcbc77b63440e6aa95fca1da8ac83e))

### Chores

- bump version to v0.5.0 ([d01e14b](https://github.com/somaz94/plainbucket/commit/d01e14ba9c1cd372898d1f0883dc3cf7eabcde9e))

### Contributors

- somaz

<br/>

## [v0.4.0](https://github.com/somaz94/plainbucket/compare/v0.3.0...v0.4.0) (2026-10-23)

### Features

- add rename, metadata editing and upload progress to the console ([4c6d5b7](https://github.com/somaz94/plainbucket/commit/4c6d5b75909c84d477066f8b92c8a11f631c5b26))

### Chores

- bump version to v0.4.0 ([deea05b](https://github.com/somaz94/plainbucket/commit/deea05bba33cbc24cf882d8d1e4ccdf56b327958))

### Contributors

- somaz

<br/>

## [v0.3.0](https://github.com/somaz94/plainbucket/compare/v0.2.0...v0.3.0) (2026-10-23)

### Features

- add console previews, filtering, selection, upload cleanup, themes ([b0162a9](https://github.com/somaz94/plainbucket/commit/b0162a9b2c0e24cff5e79c91bdbc661a8d735931))

### Bug Fixes

- sum the console usage total from the tracked bucket usage ([48361a9](https://github.com/somaz94/plainbucket/commit/48361a978d7978d401f428531e6b6ddba4942aa9))

### Chores

- bump version to v0.3.0 ([4d21825](https://github.com/somaz94/plainbucket/commit/4d218257743175566e6a691eb735150f524e364f))

### Contributors

- somaz

<br/>

## [v0.2.0](https://github.com/somaz94/plainbucket/compare/v0.1.0...v0.2.0) (2026-10-23)

### Features

- add a web console that signs S3 requests in the browser ([ab7e7c9](https://github.com/somaz94/plainbucket/commit/ab7e7c9c4fddff30d7fa691492773209140e1d61))
- add ListObjectVersions, GetObjectAttributes, partNumber reads and multipart checksums ([3c1ca88](https://github.com/somaz94/plainbucket/commit/3c1ca88a8c317a15d75a87503078320412148511))

### Bug Fixes

- **helm:** keep the test pod so helm test --logs can read it ([fa81754](https://github.com/somaz94/plainbucket/commit/fa81754ff6dcd590aa1cc0d2bf59b736e787b5c1))

### Builds

- **deps:** bump docker/login-action from 3 to 4 (#3) ([#3](https://github.com/somaz94/plainbucket/pull/3)) ([d0ea7b5](https://github.com/somaz94/plainbucket/commit/d0ea7b55fba6a16c7ae2d7c7403338862d40da24))
- **deps:** bump docker/setup-qemu-action from 3 to 4 (#2) ([#2](https://github.com/somaz94/plainbucket/pull/2)) ([1eb7c81](https://github.com/somaz94/plainbucket/commit/1eb7c813e8048b16c0ea5f52ae7e7c85af6176c4))

### Continuous Integration

- run a pinned ceph/s3-tests subset against a committed pass list ([14871ca](https://github.com/somaz94/plainbucket/commit/14871ca4a10f091136c2f676387b5db798bdcfd1))

### Chores

- bump version to v0.2.0 ([615e924](https://github.com/somaz94/plainbucket/commit/615e924235a28d1147623f7c46cbb05bf2d2f634))

### Contributors

- somaz

<br/>

## [v0.1.0](https://github.com/somaz94/plainbucket/releases/tag/v0.1.0) (2026-10-22)

### Features

- add healthcheck and fsck commands, an access log and a root lock ([28efc16](https://github.com/somaz94/plainbucket/commit/28efc167841ac59479a23860bef24ad3c8e35631))
- per-bucket expiry by last use and size limits ([f885241](https://github.com/somaz94/plainbucket/commit/f885241a73ead67c8c25cfe859c3906375f642cf))
- per-bucket settings with static website hosting and CORS ([4245622](https://github.com/somaz94/plainbucket/commit/4245622b64162af3afe8ccedfcdfea5304d4ee5b))
- **helm:** add Helm chart and wire chart and runner tests into CI ([8d1fea0](https://github.com/somaz94/plainbucket/commit/8d1fea001f4697e392d25b6f38bc68d0f817cc67))
- add scoped access keys, public buckets and virtual-hosted style ([62e75d2](https://github.com/somaz94/plainbucket/commit/62e75d2bcc7fd3ce3df0ae6a5c9077378fa2a171))
- **sigv4:** accept HEAD on GET-presigned URLs (GitLab Runner cache) ([060f645](https://github.com/somaz94/plainbucket/commit/060f645b092138a9a0fd0cd53f423ee432a0a522))
- add Prometheus metrics and cut uploads that stop making progress ([7f3ae75](https://github.com/somaz94/plainbucket/commit/7f3ae7576e7bea96cef9dcc58fb507a4df49ec71))
- add plainbucket, an S3-compatible server storing plain files ([8d99523](https://github.com/somaz94/plainbucket/commit/8d9952379a96d47e588c062170f609088a0a8b42))

### Bug Fixes

- redact presigned signatures in the access log and harden bucket limits ([3244f35](https://github.com/somaz94/plainbucket/commit/3244f35182529f25761343a80483dc1918f2385e))
- support server-side copy of large objects with UploadPartCopy ([4f0c870](https://github.com/somaz94/plainbucket/commit/4f0c8707073ead50ade7ee0190f5957d009fb5ed))
- **docker:** create /data owned by the runtime user ([1e315f4](https://github.com/somaz94/plainbucket/commit/1e315f46f3c8d2e2747de14292b6479650cc01ea))

### Documentation

- document metrics, tested clients and the container user ([90c5fe0](https://github.com/somaz94/plainbucket/commit/90c5fe090efdc123e4f960c609e9901f95233145))

### Tests

- **helm:** match schema errors by property name across helm versions ([acfbd85](https://github.com/somaz94/plainbucket/commit/acfbd85fc716347ae4b8adbeeb3ac81c146e4310))
- add gitlab-runner cache helper end-to-end script ([24f9f37](https://github.com/somaz94/plainbucket/commit/24f9f371c7a51e46f4905b765111578a2823ff71))
- add rclone end-to-end script and run it in CI ([38088b5](https://github.com/somaz94/plainbucket/commit/38088b593281b5e2e601b4d54987e134ae5bf698))

### Builds

- **deps:** bump distroless/static-debian13 from `e2e927e` to `d620678` (#1) ([#1](https://github.com/somaz94/plainbucket/pull/1)) ([05cceae](https://github.com/somaz94/plainbucket/commit/05cceaefe7322ce6472bab79c2c7cf9dea7f2215))

### Continuous Integration

- install a pinned rclone release for the end-to-end test ([db9c234](https://github.com/somaz94/plainbucket/commit/db9c23428747421cae17367e96fc071858c0055d))

### Contributors

- somaz

<br/>

