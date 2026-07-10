# syntax=docker/dockerfile:1
#
# TraceForge images. Build from the repository root:
#
#   docker buildx build -f deploy/docker/Dockerfile --target server -t traceforge/server .
#   docker buildx build -f deploy/docker/Dockerfile --target agent  -t traceforge/agent  .
#
# Four targets: server, agent, metricsctl (pure Go, static, distroless) and
# agent-pcap (CGo against libpcap, statically linked against musl).
#
# ---------------------------------------------------------------------------
# The three mistakes this file exists to not make.
#
# 1. `FROM golang AS builder` in a multi-platform build makes Docker fetch the
#    *target*'s golang image and run the compiler under QEMU. Compiling amd64 Go
#    on an arm64 laptop then takes minutes instead of seconds, for no reason: Go
#    is a cross-compiler. `--platform=$BUILDPLATFORM` pins the builder to the
#    machine doing the building, and GOARCH does the rest. This is the single
#    biggest speedup available in a Go Dockerfile, and most examples get it wrong.
#
# 2. `GOARCH=${TARGETARCH}` without a preceding `ARG TARGETARCH` expands to the
#    empty string, and `go build` cheerfully builds for the host. The image is
#    then mislabelled rather than broken, which is worse. ARGs do not cross FROM
#    boundaries, so every stage that reads one declares it.
#
# 3. An `ARG` declared in a stage that contains no `RUN` changes nothing. A
#    tempting shape here is one parameterised build stage inherited three times
#    with a different CMD_PATH — but the compile already happened in the parent,
#    so all three children would ship the same binary, correctly labelled and
#    silently wrong. Hence: one build stage, three binaries, three COPYs.
# ---------------------------------------------------------------------------

ARG GO_VERSION=1.26

# ===========================================================================
# Stage: deps — the module cache, in a layer of its own.
#
# go.mod and go.sum change far less often than the source does. Copying them
# first means editing a .go file re-runs the compile but not the download.
# ===========================================================================
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS deps

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download

# ===========================================================================
# Stage: build — every pure-Go binary, cross-compiled, static, stripped.
# ===========================================================================
FROM deps AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

COPY . .

# CGO_ENABLED=0 is what makes the binary static, and static is what makes
# distroless possible: those images have no dynamic loader, so a binary that
# wants ld.so dies with "no such file or directory" — naming the loader, not the
# binary, which is the most confusing error in containers.
#
# -trimpath strips the builder's absolute paths out of the binary. They are a
# small information leak (they name the build machine's directory layout) and the
# main obstacle to a reproducible build.
#
# -s -w drop the symbol table and DWARF. The binary shrinks by roughly a third
# and delve can no longer attach, which in production is the right trade: you
# debug with the profiler and the logs, and both still work.
#
# -buildvcs=false is explicit about something the toolchain would otherwise do
# quietly. Given a .git directory it stamps the commit into the binary — but only
# if a `git` executable is on PATH, and golang:alpine has none, so `auto` silently
# stamps nothing. Saying `false` means the version can only come from the build
# args, which is where it comes from anyway. See .dockerignore.
#
# The build cache is keyed per target platform. Without the id= the arm64 and
# amd64 builds evict each other's cache on every alternate build.
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-${TARGETOS}-${TARGETARCH},sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    LDFLAGS="-s -w \
        -X metrics-system/internal/buildinfo.version=${VERSION} \
        -X metrics-system/internal/buildinfo.commit=${COMMIT} \
        -X metrics-system/internal/buildinfo.date=${BUILD_DATE}" \
    sh -c 'set -eu; for cmd in server agent metricsctl; do \
        go build -trimpath -buildvcs=false -ldflags="$LDFLAGS" -o /out/$cmd ./cmd/$cmd; \
    done'

# An empty data directory, owned by the user the server runs as.
#
# This exists because of an interaction that is easy to hit and hard to read: a
# fresh named volume inherits the ownership of whatever the image has at its
# mount point. Without this the mount point does not exist in the image, Docker
# creates it owned by root, and the server — running as 65532 — cannot mkdir
# inside it. `permission denied` on a directory that is obviously writable.
#
# The obvious fix, `RUN mkdir -p && chown`, is not available: distroless has no
# shell, and the runtime stage cannot RUN anything. So the directory is made here,
# in a stage that does have one, and copied across with its ownership.
#
# Kubernetes solves the same problem with fsGroup, which is why the chart sets it.
RUN mkdir -p /data

# ===========================================================================
# Stage: build-pcap — the CGo agent.
#
# Note the missing --platform=$BUILDPLATFORM. This is the one stage that cannot
# cross-compile: cgo needs a C compiler and libpcap's headers for the *target*
# architecture, and the host toolchain has neither. So this stage runs under
# emulation on a foreign platform, and it is slow. That is the tax CGo levies,
# and it is why `make cross-cgo` exists to fail on purpose.
# ===========================================================================
FROM golang:${GO_VERSION}-alpine AS build-pcap

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# musl-dev, not glibc: musl links statically without complaint. glibc's static
# linking works until the first call to getaddrinfo or getpwnam, at which point
# it dlopen's an NSS module that is not there.
RUN apk add --no-cache gcc musl-dev libpcap-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .

# -extldflags=-static asks the external linker for a static binary. With musl it
# succeeds, so even the CGo agent lands on a distroless image with no libc at all.
#
# The `-version` run at the end is the smoke test that matters here: this stage
# is native to the target platform, so the binary it just linked can actually be
# executed. If libpcap resolved to a shared object, this line is where we find
# out — not in production.
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=1 \
    go build \
        -trimpath \
        -buildvcs=false \
        -ldflags="-s -w -extldflags=-static \
            -X metrics-system/internal/buildinfo.version=${VERSION} \
            -X metrics-system/internal/buildinfo.commit=${COMMIT} \
            -X metrics-system/internal/buildinfo.date=${BUILD_DATE}" \
        -o /out/agent \
        ./cmd/agent \
 && /out/agent -version

# ===========================================================================
# Runtime images.
#
# distroless/static holds ca-certificates, /etc/passwd, tzdata and nothing else:
# no shell, no package manager, no coreutils. An attacker with RCE in the process
# has no `sh` to spawn and no `curl` to fetch a second stage. It costs about 2 MB.
#
# The absence of a shell has one consequence people trip over: HEALTHCHECK CMD in
# *shell form* cannot work. The exec form can — provided something in the image
# can perform the check. That is why the binaries answer to -health-check and
# probe themselves.
# ===========================================================================

# --- server ---------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS server

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="traceforge-server" \
      org.opencontainers.image.description="TraceForge metrics server" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/ANTON-IVANOVICH/TraceForge"

COPY --from=build /out/server /usr/local/bin/server
COPY --from=build --chown=65532:65532 /data /var/lib/traceforge

# 8080 HTTP API + dashboard, 9090 gRPC, 9091 telemetry (/healthz /readyz /metrics).
EXPOSE 8080 9090 9091

# Never root. distroless's nonroot user is uid 65532; the Kubernetes manifests
# repeat the number in runAsUser so the kubelet can enforce `runAsNonRoot: true`
# without resolving a name it cannot see inside the image.
USER 65532:65532

# The container's own opinion of its health, for `docker run` and compose. In
# Kubernetes this is ignored: the kubelet uses the probes in the manifest.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/server", "-health-check"]

# ENTRYPOINT, not CMD: an operator appending arguments to `docker run` should be
# adding flags to the server, not replacing it with a program of their choosing.
ENTRYPOINT ["/usr/local/bin/server"]

# --- agent (pure Go) ------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS agent

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="traceforge-agent" \
      org.opencontainers.image.description="TraceForge metrics agent (pure Go, no packet capture)" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/ANTON-IVANOVICH/TraceForge"

COPY --from=build /out/agent /usr/local/bin/agent
EXPOSE 9101
USER 65532:65532
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/agent", "-health-check"]
ENTRYPOINT ["/usr/local/bin/agent"]

# --- agent-pcap (CGo + libpcap) -------------------------------------------
FROM gcr.io/distroless/static-debian12 AS agent-pcap

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="traceforge-agent-pcap" \
      org.opencontainers.image.description="TraceForge metrics agent with libpcap packet capture" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/ANTON-IVANOVICH/TraceForge"

COPY --from=build-pcap /out/agent /usr/local/bin/agent
EXPOSE 9101

# Deliberately root, and deliberately a separate image from `agent`.
#
# Opening a raw socket needs CAP_NET_RAW, and a capability granted by the
# container runtime lands in the *permitted* set of a root process; a non-root
# process gets it only through file capabilities or ambient capabilities, neither
# of which distroless provides. Rather than pretend, this image runs as root and
# the Helm chart will not select it unless capture is explicitly enabled. The
# default agent is the unprivileged one above.
USER 0:0
ENTRYPOINT ["/usr/local/bin/agent"]

# --- metricsctl -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS metricsctl

ARG VERSION=dev
LABEL org.opencontainers.image.title="traceforge-metricsctl" \
      org.opencontainers.image.description="TraceForge command-line client" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/ANTON-IVANOVICH/TraceForge"

COPY --from=build /out/metricsctl /usr/local/bin/metricsctl
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/metricsctl"]
