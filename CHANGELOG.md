# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.6.0] - 2026-07-09

An embedded live dashboard: a single-page app served from the binary and a
from-scratch WebSocket that streams live metrics and stats to the browser.

### Added

- `internal/server/live`: a WebSocket server implemented from scratch on the
  standard library (RFC 6455 handshake, frame codec, client-frame unmasking,
  fragmentation, ping/pong/close) — no third-party WebSocket library.
- A broadcast `Hub` (single-goroutine-owns-the-map pattern) that fans newly
  stored metrics and periodic stats snapshots out to connected dashboards, with
  non-blocking delivery (slow clients drop frames, never block the pipeline).
- Embedded SPA under `web/` (`go:embed`), served at `/` with assets at
  `/static/`; a live metric feed plus pipeline/storage counters and a chart.
- Pipeline observer hook (`SetObserver`) invoked with each stored batch.
- `-ui` / `UI` flag (default `true`) to serve the dashboard.

### Changed

- Multi-tenancy extends to the live stream: the `/ws` endpoint authenticates
  from query params (browsers can't set WS handshake headers) and scopes the
  feed to the caller's tenant; the stats stream is admin-only.
- `statusRecorder` (logging middleware) now forwards `Hijack` so the WebSocket
  upgrade works through the middleware chain.

## [0.5.0] - 2026-07-09

Authentication, role-based access control and multi-tenant data isolation across
both transports. Auth is off by default (backward compatible).

### Added

- `internal/auth`: authenticated `Principal` (subject, tenant, roles) carried
  through `context.Context`, plus an `Authenticator` chain.
- API-key authentication: `{subject, tenant, roles}` mapping loaded from a JSON
  file (`-api-keys`); keys stored only as SHA-256 hashes and resolved by hash.
- JWT bearer authentication implemented from scratch on the standard library
  (no third-party JWT lib): HS256 (`-jwt-hs256-secret`) and RS256 via a rotating
  JWKS key set (`-jwks-url`). The verifier is pinned to one algorithm (rejecting
  `none` and RS256→HS256 confusion); `exp` is mandatory; `nbf`/`iss`/`aud`
  validated; JWKS rejects sub-2048-bit RSA keys and refreshes on rotation.
- RBAC: roles `writer`/`reader`/`admin` grant actions ingest/query/admin, checked
  by an HTTP middleware and gRPC interceptors (unary + stream).
- Multi-tenancy: a server-assigned `tenant` label (never client-settable) is
  stamped on ingest and forced as a filter on every query, isolating series per
  tenant.
- Server flags `-auth`, `-api-keys`, `-jwt-hs256-secret`, `-jwks-url`,
  `-jwt-issuer`, `-jwt-audience`; agent flags `-api-key`, `-auth-token`.
- Tests: JWT (HS256/RS256, expiry, alg-confusion, issuer/audience), JWKS fetch +
  rotation + weak-key rejection, API keys, RBAC, and tenant-isolation E2E over
  both HTTP and gRPC.

### Changed

- The agent ships credentials via a `Credentials` value on both senders (HTTP
  headers / gRPC metadata).
- `model.Batch` gained a server-only `Tenant` field (`json:"-"`).

### Dependencies

- No new modules (auth is built on the standard library and existing gRPC deps).

## [0.4.0] - 2026-07-05

A gRPC + Protocol Buffers transport between agent and server, alongside HTTP.

### Added

- Protobuf-defined `metrics.v1.MetricsService` (`proto/metrics/v1/metrics.proto`)
  with all three RPC styles: `Ingest` (unary), `IngestStream` (bidirectional
  streaming) and `Query` (server streaming). Generated code lives in
  `internal/proto/metricspb` (regenerated with `make proto` or `go generate`).
- gRPC server (`internal/server/grpcserver`) that funnels batches into the same
  pipeline and store as HTTP, with panic-recovery and request-logging
  interceptors, server reflection, and a graceful-stop lifecycle mirroring the
  HTTP server.
- Agent gRPC transport: a single long-lived bidirectional stream reused across
  ticks (lockstep send/ack, reopened transparently on error), selectable with
  `-transport=grpc` / `-grpc-server`.
- `-grpc-addr` / `GRPC_ADDR` server flag (default `:9090`, empty to disable).
- `internal/grpcconv` for model <-> protobuf conversion.
- Tests: conversion round-trip, gRPC service integration over a loopback
  listener, and the agent streaming sender.

### Changed

- The agent now ships through a `Transport` interface; the HTTP sender is one
  implementation and the new gRPC streaming sender another.
- gRPC backpressure: unary `Ingest` returns `ResourceExhausted`; the stream
  replies with `IngestAck.throttled = true`.

### Dependencies

- Added `google.golang.org/grpc` and `google.golang.org/protobuf`.

## [0.3.0] - 2026-07-05

Persistent storage: two on-disk backends behind the `Storage` interface,
selectable at runtime.

### Added

- `bolt` backend (bbolt B+tree): metrics persisted with big-endian timestamp
  keys so a time-range query is a cursor range scan; writes batched into single
  transactions.
- `tsdb` backend: a from-scratch LSM-style engine — a CRC-checked write-ahead
  log (fsync + crash recovery), an in-memory head, and immutable chunks written
  atomically (fsync + rename) and read back via mmap with binary search; time
  pruning of chunks and a `flock` single-writer lock (lock-file fallback off Unix).
- `-storage` / `STORAGE` (`memory` | `bolt` | `tsdb`) and `-data-dir` / `DATA_DIR`
  to choose and locate the backend.
- Fuzz target for the chunk header parser.

### Changed

- `Storage` interface: `Write` now returns an `error`; added `WriteBatch` and
  `Close`. The pipeline's store stage batches metrics (by size or a 100ms timer)
  into `WriteBatch` calls.
- Exported shared query helpers (`SeriesKey`, `MatchLabels`, `FilterTime`,
  `ApplyQuery`) so every backend aggregates identically.

### Fixed

- Restore strict JSON decoding on ingest (reject unknown fields and trailing
  data), lost during the 0.2.0 pipeline rewrite.

### Dependencies

- Added `go.etcd.io/bbolt` (bolt backend) and `golang.org/x/sys` (mmap + flock).

## [0.2.0] - 2026-07-04

Rebuilt the server around a channel pipeline with an in-memory time-series
database and a query API.

### Added

- Channel-based ingestion pipeline (`ingest → unpack → validate → enrich → store`)
  with configurable per-stage worker pools and a bounded ingest buffer that
  drains cleanly on shutdown without losing in-flight metrics.
- In-memory time-series store with a metric-name index and canonical,
  label-sorted series keys that deduplicate series regardless of label order.
- Query API: `GET /api/v1/query` with a required metric `name`, arbitrary
  label-equality filters, and `from`/`to` time-window selection.
- Time-window aggregations: `avg`, `min`, `max`, `sum`, `count` and percentiles
  (`p50`/`p90`/`p95`/`p99`), bucketed by a configurable `step`, plus a result `limit`.
- Automatic `agent_id` label injection: the unpack stage tags every metric with
  its batch's agent id, so all series are queryable by `agent_id`.
- Per-agent token-bucket rate limiting keyed by the `X-Agent-ID` header (falling
  back to client IP), with configurable requests-per-second and burst.
- HTTP middleware chain: panic recovery, request-ID propagation (`X-Request-ID`),
  structured request logging, and rate limiting.
- Self-metrics at `GET /debug/stats` exposing pipeline counters
  (ingested/dropped/invalid/stored) and storage counters (series/points).
- Runtime profiling via `net/http/pprof` under `/debug/pprof/`.
- Configuration for ingest buffer, per-stage worker counts, and rate-limit
  RPS/burst (flags + environment variables).
- Unit tests for the new pipeline, storage, ratelimit and middleware packages;
  the server handler tests were rewritten for the pipeline.

### Changed

- Ingestion is now asynchronous: `POST /api/v1/metrics` returns `202 Accepted`
  on enqueue and `503 Service Unavailable` with a `Retry-After` header when the
  pipeline buffer is saturated (backpressure).
- The old `GET /api/v1/metrics` list endpoint is replaced by `GET /api/v1/query`.
- Error and query responses are now JSON (`{"error":"..."}`) instead of plaintext.
- The agent now sends an `X-Agent-ID` header on every batch it ships.

### Removed

- The old slice-backed in-memory storage (`internal/server/storage.go`: the
  `Storage` type with `Add`/`All`/`Count`).

### Dependencies

- Added `golang.org/x/time` v0.15.0 (`golang.org/x/time/rate`) for rate limiting.

## [0.1.0] - 2026-05-25

Initial MVP: an agent that collects host telemetry and ships it to an in-memory
collector server over HTTP.

### Added

- Agent: collects CPU usage, memory (total/used/percent), disk
  (total/used/percent for a configurable path) and uptime via `gopsutil`, on a
  configurable interval.
- Agent: runs collectors concurrently each tick with per-collector timeout and
  error isolation, then batches and POSTs the metrics as JSON.
- Reusable HTTP client (`pkg/httpx`) with timeout, connection pooling and
  automatic retries with exponential backoff on network errors, HTTP 429 and 5xx,
  safely replaying the request body between attempts.
- Server: in-memory collector with `POST /api/v1/metrics` (ingest) and
  `GET /api/v1/metrics` (list all), guarded by an `RWMutex` returning defensive
  copies.
- Hardened ingest: 1 MiB body limit, strict JSON decoding (rejects unknown
  fields and trailing data) and batch validation (`400` on bad input, `202` on
  success).
- `GET /healthz` liveness endpoint.
- Metric model: `Metric`/`Batch` with gauge/counter kinds, JSON (un)marshalling
  that accepts string or numeric type forms, and validation.
- Graceful shutdown on SIGINT/SIGTERM (the server drains in-flight requests with
  a 10s timeout).
- Configuration via flags with environment-variable fallbacks; structured JSON
  logging via `slog`.
- Unit tests across the agent, httpx, model and server packages; Makefile with
  build/test/vet/lint targets.

### Dependencies

- Go 1.26; `github.com/shirou/gopsutil/v4` for cross-platform metric collection.

[Unreleased]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ANTON-IVANOVICH/TraceForge/releases/tag/v0.1.0
