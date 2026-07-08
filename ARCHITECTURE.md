# TraceForge — Architecture

How the system is put together: components, data flow, the concurrency model,
storage internals, and the design decisions behind them. Kept in sync with the
staged roadmap.

- **Covers up to:** v0.5.0 (auth, RBAC, multi-tenancy)
- **Last updated:** 2026-07-09
- Go module: `metrics-system`. Two binaries: `agent` and `server`.

---

## 1. Big picture

```text
          ┌──────────┐   HTTP/JSON  (POST /api/v1/metrics)   ┌──────────────────────────────┐
          │  agent(s)│ ───────────────────────────────────▶ │            server            │
          │ collect+ │   gRPC/protobuf (IngestStream)        │                              │
          │  ship    │ ───────────────────────────────────▶ │  transports → auth → pipeline│
          └──────────┘                                       │        → storage ← query     │
                ▲                                             └──────────────────────────────┘
                │  (query results)   HTTP GET /api/v1/query  /  gRPC Query (server stream)
                └──────────────────────────────────────────────────────────┘
```

Two independent transports (HTTP and gRPC) accept the **same** `Batch` and feed
the **same** in-process pipeline and storage. Everything after the transport
boundary is transport-agnostic.

---

## 2. Data model (`internal/model`)

- **`Metric`** — `Name`, `Type` (gauge|counter), `Value float64`, `Timestamp`,
  `Labels map[string]string`.
- **`Batch`** — `AgentID`, `[]Metric`, and a server-only `Tenant` (`json:"-"`,
  absent from the wire and from protobuf — set from the authenticated principal,
  never trusted from the client).
- **Series identity** — a canonical, label-sorted key derived from
  `name + labels`, so a series is the same regardless of label ordering.
- Reserved, server-controlled labels: `agent_id` (from the batch) and `tenant`
  (from auth). The pipeline stamps these and strips any client-supplied copy.

---

## 3. Agent (`internal/agent`, `cmd/agent`)

```text
tick (every -interval) ─▶ collectAll ─┬─ cpu ─┐
                                       ├─ mem ─┤ concurrent, per-collector
                                       ├─ disk─┤ timeout + error isolation
                                       └─uptime┘
                                            │  []Metric
                                            ▼
                                    Batch{agent_id} ─▶ Transport.Send
```

- **Collectors** run concurrently each tick (fan-out + `WaitGroup` + closer
  goroutine); a hung collector can't stall the others (per-tick `context`
  timeout).
- **`Transport` interface** abstracts the wire: `Send(ctx, Batch) / Close()`.
  - `Sender` (HTTP) — JSON over the reusable `pkg/httpx` client (timeout,
    pooling, retry with exponential backoff on network/429/5xx).
  - `GRPCSender` — one **long-lived bidirectional stream** reused across ticks
    in lockstep (send batch → await ack), reopened transparently on error.
- **Credentials** (`-api-key` / `-auth-token`) are attached as HTTP headers or
  gRPC metadata.

---

## 4. Server

### 4.1 Request path

```text
HTTP:  mux → Recover → RequestID → Logger → RateLimit → [Authenticate] → handler
gRPC:  recover → log → [auth] interceptors → service method
                                                  │
                          (both) Batch ───────────┼──────────▶ pipeline.Ingest
                                 Query request ────┴──────────▶ storage.Query
```

- **HTTP** (`internal/server`): a `net/http` mux wrapped in a middleware chain
  (`Chain` composes `Middleware` funcs). Handlers are thin — ingest feeds the
  pipeline, reads go straight to storage.
- **gRPC** (`internal/server/grpcserver`): `metrics.v1.MetricsService` with
  `Ingest` (unary), `IngestStream` (bidirectional), `Query` (server-streaming),
  plus recovery/logging (and optional auth) interceptors and reflection.
- Both servers run concurrently under one signal-derived context; either's
  failure cancels the other. Shutdown order is strict (§4.6).

### 4.2 Pipeline (`internal/server/pipeline`)

```text
ingestCh ─▶ unpack ─▶ validateCh ═▶ validate (N) ═▶ enrichCh ═▶ enrich (N) ═▶ storeCh ═▶ store (batched)
 (Batch)   1 goroutine  (Metric)   worker pool       (Metric)   worker pool    (Metric)   → WriteBatch
```

- A chain of channel-connected stages with configurable per-stage worker pools.
- **`Ingest`** is a non-blocking send onto a bounded `ingestCh`; a full buffer is
  **backpressure** → HTTP `503` / gRPC `ResourceExhausted`, counted as `dropped`.
- **unpack** fans a batch into individual metrics and stamps the `agent_id` and
  `tenant` labels. **validate** drops malformed metrics (`invalid` counter).
  **enrich** fills a missing timestamp. **store** batches metrics (by size or a
  100 ms timer) into `WriteBatch` calls — critical for transactional backends.
- **Graceful drain:** closing `ingestCh` cascades stage-by-stage
  (each stage's closer goroutine waits for its workers, then closes the next
  channel), so no in-flight metric is lost. `sync/atomic` counters back
  `/debug/stats`.

### 4.3 Storage (`internal/server/storage`)

One interface, three backends selected by `-storage`:

```go
type Storage interface {
    Write(m Metric) error
    WriteBatch(metrics []Metric) error
    Query(q Query) ([]Metric, error)
    Stats() Stats
    Close() error
}
```

- **`memory`** — in-process series map + name index; lost on restart.
- **`bolt`** — a bbolt (B+tree) file. Nested buckets: `meta` (series→JSON) and
  `points` (a bucket per series, big-endian timestamp keys → range scans are
  cursor seeks). Writes batched into single transactions.
- **`tsdb`** — a from-scratch LSM-style engine:

  ```text
  Write ─▶ WAL (crc32 + fsync)  ─▶  in-memory head  ──flush(size/age)──▶  immutable chunk files
                                          │                                 (atomic tmp+fsync+rename)
  Query ◀── merge(head + chunks, dedup by ts) ◀── mmap + binary search ◀────┘
  ```

  WAL replay on reopen recovers an unflushed head (crash recovery). Reads mmap
  the chunk data and binary-search within it. A single-writer `flock` (O_EXCL
  lockfile fallback off Unix) guards the directory. Unix-specific bits (`mmap`,
  `flock`) have `//go:build` fallbacks.

### 4.4 Query engine

`storage.Query` = name (required) + label filter + `[from,to)` + optional
aggregator + step + limit. Shared helpers (`SeriesKey`, `MatchLabels`,
`FilterTime`, `ApplyQuery`) are exported so every backend aggregates identically:
raw points, or one aggregated value (`avg|min|max|sum|count|p50..p99`) per
non-empty `step` window.

### 4.5 Auth & multi-tenancy (`internal/auth`)

Off by default; when enabled it guards both transports through the same
`Authenticator`:

```text
credentials (X-API-Key / Bearer)
     │
     ▼
Chain ─▶ APIKeyAuthenticator (SHA-256 hash lookup)
     └─▶ JWTAuthenticator ─▶ Verifier (pinned alg)
                                 ├─ HS256 (shared secret)
                                 └─ RS256 ─▶ KeySet (JWKS: fetch, kid index, rotation)
     │
     ▼
*Principal{subject, tenant, roles}  ──context──▶  handlers / service
```

- **JWT** is verified from scratch on the stdlib. The `Verifier` is **pinned to
  one algorithm** (rejects `none` and RS256→HS256 confusion); `exp` mandatory;
  `nbf/iss/aud` checked; JWKS rejects sub-2048-bit keys and throttles refetches
  (success or failure) so a bad-`kid` flood during an outage can't pile up.
- **RBAC** — a fixed `role → actions` policy (`writer→ingest`, `reader→query`,
  `admin→all`). The HTTP middleware / gRPC interceptor map the route/method to an
  `Action`; missing credential → `401`, wrong role → `403`.
- **Multi-tenancy** — the handler/interceptor set `Batch.Tenant` from the
  principal (§2), the pipeline stamps it as the `tenant` label, and every read
  path **forces** `tenant=<caller>` onto the query, so a tenant can only read its
  own series.

### 4.6 Lifecycle

`signal.NotifyContext` (SIGINT/SIGTERM) → cancel → **both servers stop** (HTTP
`Shutdown`, gRPC `GracefulStop`) → **`pipeline.Shutdown()`** drains all in-flight
metrics → **`storage.Close()`** (flush WAL, release lock). Doing storage-close
only after the drain guarantees no data loss and no send-on-closed-channel race.

---

## 5. Configuration, logging, observability

- **Config precedence:** defaults → environment variables → flags (both binaries).
- **Logging:** structured `log/slog` JSON; every HTTP request carries an
  `X-Request-ID` propagated through context.
- **Observability:** `GET /debug/stats` (pipeline + storage counters),
  `GET /healthz` (public), `net/http/pprof` under `/debug/pprof/`.

---

## 6. Package layout

```text
proto/metrics/v1/           protobuf service + messages
cmd/{agent,server}/         entry points + flag/env config
internal/
  model/                    Metric, Batch, MetricType
  agent/                    collectors, HTTP + gRPC senders, orchestration
  auth/                     principal/RBAC, API keys, JWT (HS256/RS256), JWKS
  grpcconv/                 model <-> protobuf conversion
  proto/metricspb/          generated protobuf + gRPC code (go generate)
  server/                   HTTP handlers, middleware, auth mw, lifecycle
    grpcserver/             gRPC service, interceptors, lifecycle
    pipeline/               channel pipeline: stages, worker pools, stats
    storage/                Storage interface, query engine, memory|bolt|tsdb
    ratelimit/              per-agent token-bucket limiter
pkg/httpx/                  reusable HTTP client (retry + backoff)
```

---

## 7. Key design decisions

- **One pipeline, many transports.** Transports are thin adapters onto a shared
  `Batch` → pipeline → storage core, so HTTP and gRPC never diverge in behavior.
- **Channels over locks for ingest.** Bounded channels give natural backpressure
  and a lossless cascading drain; worker pools tune each stage independently.
- **`Storage` behind an interface.** Backends are swappable at runtime; the query
  engine is shared so aggregation is identical everywhere.
- **From-scratch TSDB and JWT.** Built on the stdlib (no third-party TSDB/JWT
  libs) — the point is to expose the mechanics (WAL/mmap/fsync;
  base64url/HMAC/RSA/claims) rather than hide them.
- **Tenant as a server-controlled label.** Isolation reuses the existing label +
  query machinery instead of a separate per-tenant store; the label is never
  client-settable.
- **Auth off by default.** New security surface is opt-in, keeping the default
  build simple and backward compatible.

---

## 8. What's next

Single-node today. The roadmap (see the wiki `Roadmap` and `CHANGELOG.md`) adds
clustering (gossip, consistent-hash sharding, Raft), a web UI, alerting, a CLI,
and deployment. This document is updated as each lands.
