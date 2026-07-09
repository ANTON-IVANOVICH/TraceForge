# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] - 2026-07-09

Testing, benchmarking and profiling raised from "as we went" to an engineering
discipline of its own. The point of the stage is not the count of new tests but
what they *found*: writing an invariant-based fuzzer and a mutation tester turns
up bugs that a green unit suite hides. Several of the fixes below are real
correctness and security defects that existed in shipped code.

### Added

- `internal/testutil`: the shared test infrastructure — object builders, custom
  assertions, an `Eventually`/`Never` poller for asynchronous conditions, a
  golden-file helper with an `-update` flag and output normalisation, and a
  **goroutine-leak detector** (`NoLeaks`) that snapshots goroutine IDs and
  reports any that outlive the test. The detector has its own tests in both
  directions, because a leak detector that silently stops detecting is the worst
  kind.
- **17 fuzz targets**, asserting invariants rather than just "does not panic":
  round-trip and injectivity for the series key; round-trip and idempotency for
  the rule parser and its `String()`; conservation for the chunk codec; the
  prefix property for WAL replay; forgery-resistance for the JWT verifier;
  round-trip for protobuf and JSON metric encoding; distinctness for the alert
  fingerprint. Every discovered crasher is committed under `testdata/fuzz/` as a
  regression seed.
- **A benchmark suite** across the hot paths (series key, storage, pipeline, rule
  evaluation, auth, WAL, chunk, table rendering), with sub-benchmarks over the
  parameters that matter and `b.ReportAllocs` throughout — allocations feed the
  GC, and the GC stops the world.
- `cmd/benchcmp` + `internal/benchcmp`: a from-scratch **benchstat**. It parses
  `go test -bench` output and reports whether a difference is statistically
  significant with a **Mann-Whitney U test** — exact for small samples, a normal
  approximation with a tie and continuity correction otherwise — so noise reads
  as `~` instead of a phantom regression. Every p-value was pinned against SciPy
  and an independent brute-force enumeration in review (agreement < 1e-9 across
  equal/unequal n, ties, and the all-identical `0 allocs/op` case). It warns when
  a file mixes GOMAXPROCS (`-8` vs `-4`) under one benchmark name.
- `cmd/mutate` + `internal/mutate`: a from-scratch **mutation tester**. It splices
  one operator at a time in the AST (`>`→`>=`, `&&`→`||`, `true`→`false`, a
  literal `+1`) by byte offset — so formatting and `//go:build` lines survive —
  and reruns the package's tests against each mutant using `go test -overlay`,
  never copying the tree. Verdicts come from `go test -json`'s structured stream,
  so a mutant whose own output mentions `[build failed]` is not misfiled; a
  cancelled run kills the test binary via its process group rather than orphaning
  it. A surviving mutant is a line the tests run but do not check.
- **The test pyramid, split by build tag.** Unit tests (no tag, `-race`),
  integration tests (`//go:build integration`: real bbolt/tsdb files, `httptest`,
  gRPC over `bufconn`, WAL crash/torn/corrupt recovery, tenant isolation against
  a real store), and an **e2e suite** (`//go:build e2e`, `test/e2e/`) that builds
  the real binaries and runs them as separate processes on `:0` ports, reading
  the bound address back from the server's structured log.
- **`Makefile`**: `test-unit|test-integration|test-e2e|test-all`, `cover`,
  `fuzz`/`fuzz-long`, `bench`/`bench-save`/`bench-compare`, `mutate`, and
  `profile-cpu|heap|trace`.
- **CI** (`.github/workflows`): the pyramid per push (lint, unit+coverage,
  integration, e2e, a 15s fuzz smoke, and an informational `benchcmp` on PRs);
  a separate workflow for 10-minute fuzzing and mutation testing, run on demand
  (`workflow_dispatch`) — its nightly cron is committed but commented out, so a
  fork does not spend Actions minutes unattended.
- A **separate pprof listener** (`-pprof-addr`, off by default) and optional
  mutex/block profile sampling (`-mutex-profile-fraction`, `-block-profile-rate`).
  The server now logs the addresses it actually bound, which is how the e2e suite
  discovers a `:0` port.

### Fixed

- **Non-injective series key (data corruption).** `SeriesKey` encoded a metric as
  `name{k=v,...}` with no escaping, so `{a: "b,c=d"}` and `{a: "b", c: "d"}` — and
  a name like `cpu{a=b}` against `cpu` with `{a: "b"}` — produced the *same* key.
  Two distinct series merged silently: points from one were returned by queries
  for the other, and the stored label set was whichever writer arrived first,
  across the memory, bolt and tsdb backends. Found by a fuzzed injectivity
  invariant. Fixed with a backslash-escaped, invertible encoding (`ParseSeriesKey`
  is the proof it cannot collide), a no-op for the clean keys real agents produce.
  Benchmark-driven optimization of the new encoder made it *faster* than the
  broken one it replaced: 4→1 allocations, −19% time at three labels.
- **Alert-fingerprint collision (alerts silently suppressed).** The same defect
  class in `alert.Fingerprint`: the label separator was neither escaped nor
  length-framed, so two different alerts could share a fingerprint and one would
  suppress the other in the dedup and grouping paths. Fixed by length-prefixing
  every hashed field.
- **Unbounded allocation in WAL replay (crash-loop DoS).** `Replay` read a
  `uint32` record length off the wire and did `make([]byte, length)`, so a torn or
  hostile header of `0xFFFFFFFF` allocated 4 GiB on startup. Now bounded by
  `maxRecordSize`, treating an oversized length like any other corrupt record.
- **int64 overflow in the chunk bounds check.** A corrupt `index.json` with a
  length near `math.MaxInt64` overflowed the `offset+length` guard, wrapped
  negative, passed the check and panicked with an out-of-range slice. Rewritten to
  compare each term against the data length separately.
- **NaN/Inf accepted over gRPC, rejected over HTTP.** `encoding/json` cannot carry
  a non-finite float, so the HTTP path rejected it at decode; protobuf carries it
  fine and `Metric.Validate` did not check, so a gRPC client could store a value
  an HTTP client could not. Closed at the shared `Validate` gate.
- **Unbounded per-agent map in the rate limiter (memory-exhaustion DoS).** One
  token bucket per key with no eviction, and the key is an attacker-chosen agent
  id or client IP: a stream of distinct keys grew the map until the process was
  OOM-killed, every request inside its own limit. Fixed with an idle sweep plus a
  hard cap and amortised batch eviction.
- **Storage write failures vanished from the pipeline counters.** A failed
  `WriteBatch` dropped its metrics silently; `ingested` would exceed
  `stored + invalid` with no counter to say by how much. Added a `failed` counter
  (and a dashboard tile), restoring the conservation identity
  `ingested == stored + invalid + failed`.

### Security

- pprof is no longer served on the API listener. `/debug/pprof/cmdline` exposes
  the process's argv (which can hold `-jwt-hs256-secret`) and `/debug/pprof/heap`
  exposes stored data; both now require an operator to bind `-pprof-addr`, and a
  non-loopback bind is warned about.
- The rate-limiter and series-key/fingerprint fixes above are each a
  denial-of-service or data-integrity issue closed in shipped code.

## [0.8.0] - 2026-07-09

`metricsctl`: a command-line client. Until now every interaction went through
curl or the browser, which is fine for a demo and useless for operating a
system. A CLI is a UI — if it is good, people use the product; if it is bad,
they wrap it in scripts and suffer. This one borrows kubectl's shape on purpose.

### Added

- `cmd/metricsctl` + `internal/cli`: a Cobra command tree — `query`, `stats`,
  `rules` (list/get/apply/preview/delete), `alerts list [--watch]`, `silences`
  (list/create/delete), `agents list`, `config`, `completion`, `version`.
- **Named contexts** (`internal/cli/config`), kubectl-style: one binary
  addresses production and staging without an edit in between. The file lives at
  `$METRICSCTL_CONFIG`, `$XDG_CONFIG_HOME/metricsctl/config.yaml` or
  `~/.metricsctl/config.yaml`, supports `${VAR}` expansion and `token-file`
  indirection, resolves `~` and config-relative paths, and is written `0600`.
  `metricsctl config view` redacts credentials unless `--show-secrets`.
- **Output formats** (`internal/cli/output`): `table` (hand-aligned, upper-cased
  headers, colour-aware column widths), `json` and `yaml` (which encode the raw
  API object, never the lossy table projection — a list stays a list), and `name`
  (one identifier per line, for `xargs`). Chosen with `-o`.
- **Declarative `rules apply -f`**: multi-document YAML, `-f -` for stdin,
  `--dry-run`, and a diff-style `created`/`updated`/`unchanged` report.
  `metadata.name` is the rule's stable id, so re-applying an unchanged file
  writes nothing — safe to run from CI on every push. The manifest is the desired
  state in full: a field it omits is reconciled back to the server's default,
  including a `for` clause the expression itself carries. Rules are compiled
  client-side before any request, so `--dry-run` catches a bad expression on the
  update path too, where the server is never asked.
- **`rules preview`**: backtest an expression over historical data before saving.
- **`alerts list --watch`**: redraw on an interval, clean exit on Ctrl+C.
- **Dynamic shell completion** for bash, zsh, fish and powershell:
  `metricsctl rules get <TAB>` asks the server which rules exist.
- **POSIX exit codes** as an API: `0` success, `1` generic, `2` usage, `3` auth,
  `4` not found — so `metricsctl rules get foo || handle_missing` works. Cobra's
  own argument and flag validation is wrapped so it exits `2`, an unknown or
  mistyped (sub)command is a usage error rather than a help screen and a silent
  `0`, and `rules apply` joins its per-rule failures so a `401` still exits `3`.
- **`agents list`**, derived from a heartbeat metric (`uptime_seconds` by
  default), because the server keeps no agent registry; documented as such.
- `make build` now also builds `bin/metricsctl`, with the version injected via
  `-ldflags -X main.version=$(git describe)`; `make install-ctl` installs it.
- `examples/alerting/rules.yaml` — a manifest for `rules apply`.

### Security

- The config is written through a fresh temporary file and renamed into place, so
  it is always `0600` and never half-written. (`os.WriteFile` applies its mode
  only when it *creates* the file: rewriting an already world-readable config
  would have left it world-readable with a new secret inside.) A loose-permissions
  file is warned about on every invocation.
- A context may configure exactly one credential, and `--api-key`/`--token`
  *replace* the context's rather than adding to it — otherwise the context's API
  key would ride along with the flag's bearer token to whatever `--server` now
  points at.
- Only the `${VAR}` placeholder form is expanded in the config. `os.ExpandEnv`
  would also eat the bare `$VAR` form, silently truncating any credential that
  contains a dollar sign.
- Colour and interactive prompts require a real terminal, detected with the
  terminal ioctl rather than the file mode — `/dev/null` is a character device
  too, and treating it as a terminal would write escape codes into redirected
  output and prompt where nobody can answer.
- Destructive commands (`rules delete`, `silences delete`) refuse to run
  unattended without `--yes`, instead of either hanging on a prompt or silently
  proceeding.
- `NO_COLOR` (see <https://no-color.org>) and `--no-color` always disable colour.

### Dependencies

- `github.com/spf13/cobra` — the point of this stage. Kept minimal otherwise:
  Viper, go-pretty and survey are replaced by the standard library (a hand-written
  table aligner and config loader, plain prompts), and `golang.org/x/term`
  supplies the one thing the stdlib cannot, terminal detection.
  `gopkg.in/yaml.v3` parses the config and rule manifests.

## [0.7.0] - 2026-07-09

Alerting: the system stops being purely passive (store and show) and becomes
active — it watches its own data, decides when something is wrong, and tells
someone. Rule evaluation and notification delivery are separated by a channel,
because one is periodic and deterministic while the other talks to services that
time out, rate-limit and fall over.

### Added

- `internal/alerting/rules`: a **PromQL-lite rule DSL** with a hand-written lexer
  and a recursive-descent parser (one function per grammar production), an AST
  and its evaluator. Comparisons *filter* (`cpu > 90` yields the breaching
  samples); range functions (`rate`, `increase`, `delta`,
  `{avg,min,max,sum,count,last,stddev}_over_time`) with counter-reset handling;
  instant functions (`abs`, `ceil`, `floor`, `round`, `clamp_min`, `clamp_max`);
  aggregations (`sum|avg|min|max|count|stddev`) with `by`/`without`; `and`, `or`,
  `unless`; label matchers `=`, `!=`, `=~`, `!~` with fully anchored regexes.
  Parse errors carry a byte position; input length, regex length and recursion
  depth are bounded.
- The **alert state machine** (`inactive → pending → firing → resolved`) with
  `for` semantics: an alert fires only after the condition has held
  *continuously*. Resolutions are always announced — including when the rule that
  produced them is deleted or disabled, and never for an alert that was silenced
  or inhibited and therefore never announced in the first place. Alert identity is
  a stable fingerprint over the rule ID plus sorted labels, so re-evaluation
  dedups instead of re-paging. Annotations are `text/template` expanded over the
  alert's value and labels.
- A **scheduler** (`rules.Manager`): one goroutine per rule ticking on its own
  interval, with a randomised start delay (so rules loaded together do not
  stampede storage), a per-iteration timeout, and hot reload without a restart.
- `internal/alerting/alert`: **grouping and dedup** — one notification for fifty
  failed hosts. `group_wait` / `group_interval` / `repeat_interval` scheduling,
  with a content hash so an unchanged group is not re-sent.
- `internal/alerting/silence`: **silences** (mute matching alerts for a window)
  with `=`, `!=`, `=~`, `!~` matchers; `internal/alerting/inhibit`: **inhibition
  rules** (a firing `HostDown` suppresses `CPUHigh` on the same host).
- `internal/alerting/notify`: the dispatcher, a **retry queue** with exponential
  backoff **and jitter** (a shared webhook must not be hit by a thundering herd
  of synchronised retries), and a **circuit breaker** per receiver — lock-free on
  the hot path, admitting exactly one probe while half-open.
- `internal/alerting/notify/receivers`: `log`, `webhook` (HMAC-signed with a
  signed timestamp against replay), `slack` (incoming webhook), and `email`
  (`net/smtp`, header-injection safe). Permanent failures (4xx other than
  408/429) are never retried.
- `internal/clock`: an injectable `Clock` (`Real` + a deterministic `Fake` with
  `Advance`/`BlockUntil`), so the time-heavy alerting logic is tested without
  sleeps.
- Tenant-scoped alerting API: `GET|POST /api/v1/rules`,
  `GET|PUT|DELETE /api/v1/rules/{id}`, `POST /api/v1/rules/preview` (backtest an
  expression over historical data without saving it), `GET /api/v1/alerts`,
  `GET|POST /api/v1/silences`, `DELETE /api/v1/silences/{id}`.
- Flags: `-alerting`, `-alert-rules`, `-alert-config`, `-alert-lookback`,
  `-alert-buffer` (plus the matching env vars). Off by default.
- `examples/alerting/{rules,receivers}.json` — a runnable sample configuration.

### Changed

- The live dashboard gained an **alerts panel**; the hub pushes tenant-scoped
  `alert` events over the existing WebSocket.
- RBAC extends to alerting: reading rules/alerts/silences needs the `query`
  action, mutating them needs `admin`.
- Multi-tenancy extends to alerting: a rule evaluates through a querier that
  force-injects `tenant=<owner>` into every storage query, and a tenant can
  neither see nor modify another tenant's rules, alerts or silences. A rule's
  tenant comes from the authenticated principal, never from the request body.
- `cmd/server`: the alerting service runs alongside the HTTP and gRPC servers
  and stops with them, before the pipeline drains and storage is closed.

### Security

- Webhook payloads are signed `sha256=HMAC(secret, "<unix-ts>.<body>")`; signing
  the timestamp is what stops a captured request from being replayed later.
- `tenant` is a reserved rule label. It is rejected at rule creation and
  re-stamped from the rule's owner during evaluation, so a rule can neither forge
  the tenant attribution of its alerts nor lose it to an aggregation such as
  `max by (agent_id) (…)`.
- Silences are applied at delivery time as well as on ingest, so a silence
  created after an alert was grouped still suppresses its repeat reminders.
- Email headers are sanitised, so a newline smuggled through a label cannot
  inject extra headers. The SMTP conversation runs on a connection with a dial
  timeout and an absolute deadline, and is closed on context cancellation —
  `smtp.SendMail` sets neither, so a peer that accepts and then goes silent would
  otherwise strand one goroutine per alert.
- A silence with no matchers is rejected: it would mute every alert in the system.

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

[Unreleased]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.9.0...HEAD
[0.9.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ANTON-IVANOVICH/TraceForge/releases/tag/v0.1.0
