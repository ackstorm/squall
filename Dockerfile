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
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS builder
ARG CMD=controller
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/app "./cmd/${CMD}"

FROM gcr.io/distroless/static-debian12:nonroot
USER 65532:65532
COPY --from=builder /out/app /app
ENTRYPOINT ["/app"]
