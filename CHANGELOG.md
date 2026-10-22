# Changelog

All notable changes to this project will be documented in this file.

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

