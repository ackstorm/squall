# syntax=docker/dockerfile:1.7

# Multi-stage build for squall's binaries (Phase 11 e2e infra):
#   squall-controller  (cmd/controller)
#   squall-proxy       (cmd/proxy)
#   fake-dstack        (cmd/fake-dstack, test double — never shipped as a release)
#   model-mock         (cmd/model-mock, test double — never shipped as a release)
#
# Select which one to build with --build-arg CMD=<controller|proxy|fake-dstack|model-mock>.
# No `COPY . .`: only the paths a `go build` of these binaries actually needs
# are copied, so the build context stays small and .git/secrets never reach
# the builder stage.
ARG GO_VERSION=1.27

# The builder runs on the build host's platform and cross-compiles for
# TARGETARCH, so a multi-arch build needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETOS TARGETARCH
ARG CMD=controller
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/app "./cmd/${CMD}"

FROM gcr.io/distroless/static-debian12:nonroot
USER 65532:65532
COPY --from=builder /out/app /app
ENTRYPOINT ["/app"]
