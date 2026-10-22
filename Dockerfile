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
    -o plainbucket ./cmd/ && \
    mkdir -p /out/data

# Pinned by Debian release and digest: the unversioned static tag moves to a new Debian
# release silently, and Dependabot can only bump what is pinned.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
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
# Owned by the runtime user: an unmounted VOLUME would otherwise be created root-owned.
COPY --from=builder --chown=65532:65532 /out/data /data

VOLUME ["/data"]
EXPOSE 9000

USER 65532:65532

ENTRYPOINT ["/plainbucket"]
