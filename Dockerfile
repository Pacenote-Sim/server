# syntax=docker/dockerfile:1

# Pacenote community edition — the server, as a container.
#
# The build context is the workspace directory that holds the repositories, not
# this one, because this module imports github.com/pacenote-sim/protocol and
# github.com/pacenote-sim/plugin and neither is published yet:
#
#   docker build -f server/Dockerfile -t pacenote-server:dev ..
#
# `make image` does that for you. The go.work this needs is written here rather
# than copied, so the build does not depend on a file that is deliberately not
# committed.
#
# The result is a distroless image: no shell, no package manager, no libc to
# link against, one static binary and one writable directory. It runs the setup
# wizard on first start exactly as the downloaded binary does — give it a
# database in PACENOTE_DATABASE_URL and read the token from the container log.

ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# --- build ------------------------------------------------------------------

FROM ${GO_IMAGE} AS build

WORKDIR /src

# The manifests first, so a source-only change does not re-download the module
# graph. go.work is written rather than copied: the committed tree has none.
COPY protocol/go.mod protocol/go.sum ./protocol/
COPY plugin/go.mod plugin/go.sum ./plugin/
COPY server/go.mod server/go.sum ./server/
RUN go work init ./protocol ./plugin ./server

RUN --mount=type=cache,target=/go/pkg/mod \
    GOWORK=off go -C protocol mod download && \
    GOWORK=off go -C plugin mod download && \
    GOWORK=off go -C server mod download

COPY protocol/ ./protocol/
COPY plugin/ ./plugin/
COPY server/ ./server/

# TARGETOS and TARGETARCH come from buildx, so `--platform linux/arm64` builds
# an arm64 binary on an amd64 runner without a cross-compiler — the binary is
# static and CGO is off, so there is nothing to cross-compile with.
ARG TARGETOS
ARG TARGETARCH
# The version an operator sees in the panel and in -version. There is no .git in
# this context to read a tag from, which is the one case internal/buildinfo lets
# the linker answer.
ARG VERSION=0.0.0-dev

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go -C server build \
        -trimpath \
        -buildvcs=false \
        -ldflags "-s -w -X github.com/pacenote-sim/server/internal/buildinfo.Stamp=${VERSION}" \
        -o /out/pacenote-server ./cmd/pacenote-server

# The data directory is made here, with the right owner, because the runtime
# image has no shell to mkdir with. Docker seeds a fresh named volume from the
# image's directory, ownership included, so a mounted volume lands writable.
RUN install -d -o 65532 -g 65532 -m 0700 /out/data

# --- runtime ----------------------------------------------------------------

FROM ${RUNTIME_IMAGE}

# Re-declared inside the stage. An ARG from before the first FROM is in scope
# for the FROM line itself and nowhere else, so without this the base.name label
# below expands to an empty string — a label that says it records the base image
# and records nothing.
ARG RUNTIME_IMAGE
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="pacenote-server" \
      org.opencontainers.image.description="Pacenote community edition telemetry server" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.base.name="${RUNTIME_IMAGE}"

COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/pacenote-server /pacenote-server

# config.json, and the certificate cache if this server holds its own. Named in
# compose; an anonymous volume otherwise, so a `docker rm` does not silently
# take the encryption key for the operator's stored API key with it.
VOLUME ["/data"]

ENV PACENOTE_DATA_DIR=/data \
    PACENOTE_LISTEN=:8080 \
    PACENOTE_METRICS_LISTEN=127.0.0.1:9090

EXPOSE 8080

# The nonroot user distroless ships. It is spelled numerically so that
# `read_only: true` and a --user override agree about who owns /data.
USER 65532:65532

ENTRYPOINT ["/pacenote-server"]
