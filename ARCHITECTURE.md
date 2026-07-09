# TraceForge — Architecture

How the system is put together: components, data flow, the concurrency model,
storage internals, and the design decisions behind them. Kept in sync with the
staged roadmap.

- **Covers up to:** v0.8.0 (metricsctl CLI)
- **Last updated:** 2026-07-09
- Go module: `metrics-system`. Three binaries: `agent`, `server`, `metricsctl`.

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

### 4.6 Live dashboard (`internal/server/live`, `web`)

```text
store stage ──onStored(copy)──▶ Hub.PublishMetrics ─┐
stats ticker ─────────────────▶ Hub.PublishStats ───┤ (non-blocking; drop on slow)
                                                     ▼
                            Hub (single goroutine owns the client set)
                                                     │  per-client tenant filter
                                                     ▼
                        writePump ─▶ WebSocket text frame ─▶ browser SPA (go:embed)
```

- A from-scratch **WebSocket** (RFC 6455) on `net/http` hijack — handshake, frame
  codec, client-frame unmasking, fragmentation, ping/pong/close. `statusRecorder`
  forwards `Hijack` so the upgrade survives the middleware chain.
- The **Hub** follows the single-goroutine-owns-the-map pattern (no locks):
  register/unregister/metrics/stats flow through channels; delivery to each
  client is a non-blocking send, so a slow browser drops frames instead of
  stalling the pipeline. The pipeline feeds it via an `onStored` observer that
  copies each batch (the pipeline reuses its backing array).
- **Auth tie-in:** `/`, `/static/` and `/ws` are public at the middleware layer;
  the `/ws` handler authenticates from query params (browsers can't set WS
  handshake headers), scopes the client to its tenant, and gates the stats stream
  to admins — so the live feed honors the same isolation as the query API.
- The hub also carries **alerts** (§4.7): the notifier taps every alert it
  receives, and the hub delivers it only to clients allowed to see its tenant.
  The tap sits *before* silencing, because the dashboard should show what is
  actually wrong, not only what got delivered. `live` never imports the alerting
  packages — it is a transport, so `cmd/server` adapts the domain type into the
  hub's `AlertEvent`.

### 4.7 Alerting (`internal/alerting`)

Off by default (`-alerting`). Here the system stops being passive and starts
deciding.

```text
        ┌──────────── evaluation: periodic, deterministic ─────────────┐
RuleStore ─▶ Manager ─▶ Evaluator ─▶ [for state machine] ─▶ alerts chan
           (1 goroutine/rule,  (Querier = tenant-scoped
            jitter + timeout)   read of storage)
        └──────────────────────────────────────────────────────────────┘
                     │   the only coupling: one buffered channel
        ┌──────────── delivery: slow, unreliable, async ───────────────┐
alerts chan ─▶ Notifier ─▶ Silencer ─▶ Inhibitor ─▶ Grouper ─▶ groups chan
                                                                   │
        workers ─▶ CircuitBreaker(per receiver) ─▶ Receiver ◀───────┤
                          │ transient failure                       │
                          ▼                                         │
                   RetryQueue (backoff + jitter) ───────────────────┘
        └──────────────────────────────────────────────────────────────┘
```

**Why the split.** Evaluation must not miss a tick and is storage-bound.
Delivery talks to SMTP, Slack and webhooks, which time out and rate-limit.
Wiring them directly would let one slow receiver stall rule evaluation.

- **Rule DSL** (`rules/{lexer,parser,expression}.go`) — a hand-written lexer and
  a **recursive-descent parser**: one method per grammar production, so the call
  graph *is* the grammar and precedence is legible instead of table-driven.
  Comparison has **filter semantics** (`cpu > 90` evaluates to the samples that
  breach it) — that is what turns a vector into an alert set. Range selectors
  (`cpu[5m]`) are legal only inside range functions, checked at *parse* time so a
  malformed rule is rejected when created rather than at 3am. Input length, regex
  length and recursion depth are bounded.
- **State machine** (`rules/evaluator.go`) — `inactive → pending → firing →
  resolved`. `for` requires a **continuous** breach: a lapse resets `ActiveAt`.
  Resolutions are always emitted (alerting without resolve is just spam). Alert
  identity is `Fingerprint(ruleID, sorted labels)`, stable across evaluations and
  restarts — an unstable fingerprint would turn every evaluation into a new page.
  A still-firing alert is re-emitted only every `ResendDelay`; resolved state is
  kept for a grace period (so a flap is recognised as a resurrection, not a new
  alert) and then dropped, so the store stays bounded.
- **Scheduler** (`rules/manager.go`) — one goroutine per rule ticking on its own
  interval, with a **random start delay** (otherwise every rule loaded at boot
  evaluates on the same instant forever, turning steady read load into a periodic
  burst) and a per-iteration timeout. `Apply` hot-reloads a rule by stopping and
  awaiting its old runner under the lock, so a rule never has two runners.
- **Grouping** (`alert/grouper.go`) — alerts are batched by `group_by` labels,
  per receiver. `group_wait` lets an incident coalesce (fifty hosts fail over
  ~30s, not simultaneously); `group_interval` bounds updates; `repeat_interval`
  re-sends an unchanged group as a reminder. A content hash over sorted
  (fingerprint, status) pairs decides whether anything actually changed.
- **Silences** (`silence/`) and **inhibition** (`inhibit/`) filter alerts before
  grouping. Both are tenant-safe: a silence or a source alert of tenant A cannot
  affect tenant B. A silence with no matchers is rejected — it would mute
  everything.
- **Delivery** (`notify/`) — a worker pool over groups; one **circuit breaker per
  receiver** (lock-free hot path via `sync/atomic`, mutex only for the rare
  transitions, exactly one probe admitted while half-open) so a dead SMTP server
  cannot accumulate a thousand goroutines stuck on a dial timeout; a **retry
  queue** (`container/heap` keyed by next-attempt time) with exponential backoff
  **and jitter** — without jitter a thousand tenants retry a shared webhook on the
  same 1s/2s/4s boundaries and recreate the overload they are backing off from.
  Permanent failures (4xx other than 408/429) are dropped, never retried.
- **Receivers** — `log`, `webhook` (HMAC over `"<timestamp>.<body>"`, so a
  captured request cannot be replayed), `slack`, and `email` (`net/smtp` takes no
  context, so the call runs on its own goroutine and `Send` returns when the
  context expires; headers are sanitised against injection).
- **Tenancy** — `rules.StorageQuerier` force-injects `tenant=<owner>` into every
  storage query, so a rule is *structurally* unable to observe another tenant's
  series no matter what its expression asks for. A rule's `TenantID` and a
  silence's owner come from the authenticated principal, never the request body;
  a foreign ID reads as `404`, not `403`, so IDs cannot be probed.
- **Testability** — `internal/clock` injects time (`Real`, and a `Fake` with
  `Advance`/`BlockUntil`). Alerting is defined almost entirely in terms of
  durations; testing it against the wall clock would be slow and flaky.

### 4.8 Lifecycle

`signal.NotifyContext` (SIGINT/SIGTERM) → cancel → **HTTP, gRPC and alerting stop**
(HTTP `Shutdown`, gRPC `GracefulStop`; rule runners are cancelled, then the
notifier drains) → **`pipeline.Shutdown()`** drains all in-flight metrics →
**`storage.Close()`** (flush WAL, release lock). Alerting reads storage, so it
stops before the store closes; the pipeline drains only once every writer is gone.
That ordering is what guarantees no data loss and no send-on-closed-channel race.

---

## 4b. CLI (`internal/cli`, `cmd/metricsctl`)

`metricsctl` is a Cobra command tree over the server's HTTP API. Its shape is
kubectl's — `noun verb`, persistent flags, named contexts, `-o json` — because a
CLI is a UI, and a familiar one gets used instead of wrapped in shell scripts.

```text
cmd/metricsctl ─▶ cli.NewRootCmd ─▶ PersistentPreRunE: setup()
                                        │ load config, resolve context,
                                        │ apply flag overrides, build printer
                                        ▼
                                    cli.Context  ──(context.Context)──▶ every RunE
                                     │  Client() (lazy)   Printer   Color
                                     │  Stdout/Stderr/Stdin
                                     ▼
                        client.Client ──HTTP──▶ server API
```

- **Dependency injection through `context.Context`.** `setup` builds one
  `cli.Context` and stashes it; each `RunE` pulls it back out. The API client is
  built lazily, so `config get-contexts`, `completion` and `version` need no
  reachable server.
- **Streams are fields, not globals.** No command touches `os.Stdout`. That is
  the entire reason the command tree is testable against a `bytes.Buffer` and an
  `httptest.Server`, with golden files pinning the table layout.
- **Contexts** (`cli/config`) are the kubeconfig model: a server, a credential,
  optional TLS. Written `0600`; `${VAR}` expanded from the environment so a
  checked-in config carries placeholders rather than secrets; `~` and
  config-relative paths resolved, because YAML will not do it for you.
- **Printers** (`cli/output`) separate the two audiences. `table` and `name` use
  the human projection; `json` and `yaml` encode the **raw API object**, so
  machine output never inherits a table's lossiness, and a list stays a list. The
  table is aligned by hand rather than with `text/tabwriter`, which would count a
  cell's ANSI colour bytes as visible width.
- **Exit codes are an API**: `0/1/2/3/4` for ok/generic/usage/auth/not-found.
  Cobra's own argument and flag validation is wrapped (`usageArgs`,
  `SetFlagErrorFunc`) so those failures exit `2` rather than `1`; a noun command
  such as `rules` is given a `Run` so a mistyped subcommand is a usage error
  instead of a help screen and a silent success.
- **Terminal manners.** Colour and prompts require a real terminal, detected with
  the terminal ioctl (`golang.org/x/term`) — a file-mode check would call
  `/dev/null` a terminal, since it is a character device. `NO_COLOR` and
  `--no-color` win; a destructive command without a TTY needs `--yes`, so a
  script neither hangs nor silently destroys.
- **`rules apply` is declarative and idempotent.** `metadata.name` is the rule's
  id. The CLI compiles the manifest with the *server's own* rule package, so it
  knows the state the server would store — defaults filled in, a `for` clause
  lifted out of the expression — then compares and writes only on a real
  difference, reporting `created`/`updated`/`unchanged`. That is what lets an
  omitted field mean "reset to the default" without making every apply rewrite
  the rule, and what gives `--dry-run` teeth on the update path.
- **Dependencies.** Cobra is the point of the stage. Viper, go-pretty and survey
  are not: a hand-written aligner and loader and plain prompts do the same work
  with less surface, in keeping with the rest of the project.

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
cmd/{agent,server,metricsctl}/  entry points + flag/env config
internal/
  model/                    Metric, Batch, MetricType
  agent/                    collectors, HTTP + gRPC senders, orchestration
  auth/                     principal/RBAC, API keys, JWT (HS256/RS256), JWKS
  clock/                    injectable time: Real + deterministic Fake
  cli/                      metricsctl: cobra tree, CLI context, errors/exit codes
    config/                 kubectl-style contexts + credentials
    client/                 HTTP client for the server API
    output/                 table/json/yaml/name printers, TTY + NO_COLOR
  grpcconv/                 model <-> protobuf conversion
  proto/metricspb/          generated protobuf + gRPC code (go generate)
  alerting/                 service assembly + tenant-scoped alerting API
    rules/                  DSL (lexer, parser, AST), evaluator, stores, scheduler
    alert/                  alert model, grouping, dedup
    silence/                silences + label matchers
    inhibit/                alert suppression rules
    notify/                 dispatcher, retry queue, circuit breaker
      receivers/            log, webhook (HMAC), slack, email
    config/                 receivers/routing + bootstrap rules loading
  server/                   HTTP handlers, middleware, auth mw, lifecycle
    grpcserver/             gRPC service, interceptors, lifecycle
    pipeline/               channel pipeline: stages, worker pools, stats
    storage/                Storage interface, query engine, memory|bolt|tsdb
    live/                   from-scratch WebSocket + broadcast hub
    ratelimit/              per-agent token-bucket limiter
examples/alerting/          sample rules + receivers configuration
web/                        embedded dashboard SPA (go:embed)
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
- **From-scratch TSDB, JWT, WebSocket and rule parser.** Built on the stdlib (no
  third-party TSDB/JWT/WebSocket/parser-generator libraries) — the point is to
  expose the mechanics (WAL/mmap/fsync; base64url/HMAC/RSA/claims; RFC 6455
  framing; recursive descent) rather than hide them.
- **Evaluation and delivery never share a goroutine.** Alerting's two halves have
  opposite properties — one deterministic and periodic, one slow and failure-prone
  — so they are joined only by a buffered channel, with a circuit breaker and a
  retry queue absorbing the failures on the far side.
- **Time is injected.** `internal/clock` exists because alerting is defined in
  durations (`for 5m`, `group_wait`, backoff); a test that sleeps is slow and flaky.
- **Tenant as a server-controlled label.** Isolation reuses the existing label +
  query machinery instead of a separate per-tenant store; the label is never
  client-settable.
- **Auth off by default.** New security surface is opt-in, keeping the default
  build simple and backward compatible.

---

## 8. What's next

Single-node today. The roadmap (see the wiki `Roadmap` and `CHANGELOG.md`) adds a
testing/benchmarking/profiling deep-dive, CGo integration, and a production
deployment. Clustering (gossip membership, consistent-hash sharding, Raft) is a
natural extension beyond the staged plan. This document is updated as each lands.

Known limitations of the alerting subsystem, deliberately deferred: the rule,
state and silence stores are in-memory (a restart forgets pending retries and
silences, and re-arms `for` windows), and there is no escalation, acknowledgement
or recurring maintenance window.
