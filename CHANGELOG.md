# Changelog

All notable changes to this project will be documented in this file.

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

