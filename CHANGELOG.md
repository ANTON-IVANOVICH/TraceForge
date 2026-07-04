# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Restore strict JSON decoding on ingest (reject unknown fields and trailing
  data), lost during the 0.2.0 pipeline rewrite.

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

[Unreleased]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ANTON-IVANOVICH/TraceForge/releases/tag/v0.1.0
