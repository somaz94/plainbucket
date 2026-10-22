# syntax=docker/dockerfile:1
# Build the plainbucket binary. The builder runs natively and cross-compiles via
# TARGETOS/TARGETARCH, so the arm64 image is not compiled under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w \
      -X github.com/somaz94/plainbucket/internal/version.Version=${VERSION} \
      -X github.com/somaz94/plainbucket/internal/version.GitCommit=${GIT_COMMIT} \
      -X github.com/somaz94/plainbucket/internal/version.BuildDate=${BUILD_DATE}" \
    -o plainbucket ./cmd/

# Pinned by Debian release and digest: the unversioned static tag moves to a new Debian
# release silently, and Dependabot can only bump what is pinned.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="plainbucket" \
      org.opencontainers.image.description="S3-compatible object server that stores objects as plain files" \
      org.opencontainers.image.url="https://github.com/somaz94/plainbucket" \
      org.opencontainers.image.source="https://github.com/somaz94/plainbucket" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

WORKDIR /
COPY --from=builder /workspace/plainbucket .

# Bucket root: every bucket is a directory here
VOLUME ["/data"]
EXPOSE 9000

USER 65532:65532

ENTRYPOINT ["/plainbucket"]
