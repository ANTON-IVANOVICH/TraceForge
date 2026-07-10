# TraceForge — Architecture

How the system is put together: components, data flow, the concurrency model,
storage internals, and the design decisions behind them. Kept in sync with the
staged roadmap.

- **Covers up to:** v0.11.0 (deployment: containers, Kubernetes, probes, `/metrics`)
- **Last updated:** 2026-07-10
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
HTTP:  mux → Metrics → Recover → RequestID → Logger → RateLimit → [Authenticate] → handler
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
- Both servers run concurrently; either's failure cancels the other. Shutdown
  order is strict (§5.2).
- The metrics middleware is the outermost, and that is deliberate: `Recover` turns
  a panic into a 500 and returns normally, so anything wrapped inside it never runs
  its post-handler code for the requests you most want in the error rate.

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
    Ping(ctx context.Context) error   // answers the readiness probe; see §5.1
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

`signal.NotifyContext` (SIGINT/SIGTERM) → **fail readiness, then wait** (§5.2) →
cancel `runCtx` → **HTTP, gRPC and alerting stop** (HTTP `Shutdown`, gRPC
`GracefulStop`; rule runners are cancelled, then the notifier drains) →
**`pipeline.Shutdown()`** drains all in-flight metrics → **`storage.Close()`**
(flush WAL, release lock) → the telemetry listener stops last. Alerting reads
storage, so it stops before the store closes; the pipeline drains only once every
writer is gone. That ordering is what guarantees no data loss and no
send-on-closed-channel race.

The readiness flip at the front and the telemetry listener at the back are there
for the load balancer and the kubelet, not for the data. See §5.2.

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

## 4d. The CGo boundary (`internal/agent/network`)

The agent's packet-capture collector is the only place this project leaves Go.
It exists because there is no way to read frames off a live interface from pure
Go without reimplementing BPF on every platform.

```text
  Go                         │  C
  ───────────────────────────┼──────────────────────────────
  Capture.Next()  ──────────▶│  tf_pcap_next  ──▶ pcap_next_ex
       ◀── C.GoBytes(copy) ──│    (returns a pointer into
                             │     libpcap's reusable buffer)
                             │
  Capture.Loop(fn) ─────────▶│  tf_pcap_loop  ──▶ pcap_loop
  tfPacketHandler ◀──────────│    callback, keyed by an
   (//export, cgo.Handle)    │    opaque integer handle
```

Four rules hold the boundary together, and each one is a bug the naive version
has:

1. **Ownership is explicit.** `C.CString` allocates in the C heap; every one is
   freed. libpcap's packet pointer aims into a buffer it overwrites on the next
   call, so every packet is copied with `C.GoBytes` — a test deletes that copy
   and watches packet one turn into packet two.
2. **Handle lifetime is a lock, not a hope.** `pcap_close` frees the handle. An
   `RWMutex` lets readers hold it while inside C and makes `Close` wait for
   them; `Close` calls `pcap_breakloop` first so a blocking read is interrupted
   rather than waited on. A `runtime.AddCleanup` (not `SetFinalizer`, which can
   resurrect its object) is the safety net for a caller who forgets.
3. **C never holds a Go pointer.** The `pcap_loop` callback is found through an
   opaque integer handle — `runtime/cgo.Handle`, which is the map-and-mutex
   registry everyone hand-rolls, already in the standard library. A panic inside
   the callback is recovered at the boundary, because unwinding through a C
   stack frame is undefined behaviour.
4. **The C lives in a `.c` file.** `struct pcap_pkthdr` embeds a
   platform-dependent `timeval`, libpcap's headers carry unions cgo cannot
   translate, and a Go file using `//export` may not *define* C functions in its
   preamble.

**Shape follows cost.** A CGo call is ~20ns against ~0.3ns for a Go call, so the
shim returns a whole packet per call rather than a field per call. Everything
that can be pure Go is: the link-layer and IP parsing (Ethernet, VLAN stacks,
BSD/OpenBSD loopback, raw IP, Linux cooked capture, IPv6 extension chains) reads
bytes chosen by whoever is on the network, and is fuzzed accordingly.

**Push meets pull.** Capture is push-shaped; the agent's collector interface is
pull-shaped. A background goroutine reads packets into atomics, and `Collect`
merely snapshots them — so a quiet network never slows the agent's tick.

### 4d.1 Build tags, and the tax CGo levies

Taking a CGo dependency costs three things: `GOOS=linux GOARCH=arm64 go build`
(the toolchain needs a C compiler and headers for the *target*), the static
binary, and the ability to build on a host without libpcap. So capture sits
behind `//go:build cgo`, with a stub behind `//go:build !cgo`, and
`CGO_ENABLED=0` still produces a complete agent whose network collector reports
itself unavailable. CI builds, vets and tests both — a build tag nobody
exercises is a build tag that rots.

### 4d.2 Why eBPF is documented and not shipped

eBPF is the right tool for kernel-level metrics: a verified program runs in the
kernel, counts events at nanosecond cost, and exposes a map user space reads.
`cilium/ebpf` reaches it from pure Go (no libbpf, no CGo), which is what Cilium,
Pixie and Pyroscope use.

It is not in this repository because it could not be compiled or run where this
stage was built: it needs Linux, `clang`, a generated `vmlinux.h`, and a kernel
to attach a kprobe to. Committing several hundred lines of Linux-only Go and
restricted C that has never been through a compiler would contradict the
discipline v0.9.0 established. When it is worth its price — Linux ≥ 5.4, `CAP_BPF`
or root, and a metric nothing else exposes — the shape is: a `SEC("kprobe/...")`
program updating a `BPF_MAP_TYPE_ARRAY`, `bpf2go` generating the loader, and a
collector that `Lookup`s the counter on each tick.

`internal/agent/kernel` ships instead, and makes the stage's real point: TCP
retransmits, resets, listen-queue overflows and UDP errors are already in
`/proc/net/snmp` and `/proc/net/netstat`. No C, no dependency, no privilege, and
it cross-compiles. **Check the alternative before you cross the border** — a pure
Go port, a direct syscall via `golang.org/x/sys/unix`, a subprocess, or WASM.

---

## 5. Configuration, logging, observability

- **Config precedence:** defaults → environment variables → flags (both binaries).
- **Logging:** structured `log/slog` JSON; every HTTP request carries an
  `X-Request-ID` propagated through context.
- **Build identity:** `internal/buildinfo` reconciles two sources. `-ldflags -X`
  carries the version a release pipeline knows; `runtime/debug.ReadBuildInfo`
  carries the commit and the dirty bit the Go toolchain stamps from VCS. ldflags
  wins field by field — a deliberate release stamp beats an inferred one — except
  for `Dirty`, which is always taken from VCS when VCS knows it, because a release
  built from a modified tree is exactly what an operator debugging a bad rollout
  needs to see, and the pipeline believes it is building a clean tag.
- **Three listeners, three jobs.** The API port (`-addr`) serves users. The
  telemetry port (`-telemetry-addr`, default `:9091`) serves `/healthz`,
  `/readyz`, `/startupz` and `/metrics`. pprof (`-pprof-addr`, off) serves nobody
  by default. They are separate because they have different threat models and
  different failure modes: `/debug/pprof/cmdline` prints argv, which is where
  `-jwt-hs256-secret` lives; `/metrics` names every tenant's alert severities; and
  a saturated API listener is exactly when the readiness probe most needs to
  answer, which it cannot do from behind the queue it is reporting on.

### 5.1 Probes (`internal/server/health`)

Three probes, and conflating any two of them is a known way to turn a degradation
into an outage.

| Probe | Answers | On failure | Checks dependencies? |
|-------|---------|------------|----------------------|
| `/healthz` | is this process wedged? | container restarts | **never** |
| `/readyz` | should traffic come here? | pod leaves the Service endpoints | yes |
| `/startupz` | has initialisation finished? | (nothing; delays liveness) | no |

- **Liveness depends on nothing.** The textbook outage is a liveness probe that
  checks the database: the database has one bad minute, every replica's liveness
  fails at the same instant, and the orchestrator restarts the fleet while the
  database is trying to recover.
- **Startup is a fact, not a timer.** `time.Since(start) < 5*time.Second` reports
  ready before a large WAL has replayed, and not-ready when the replay runs long.
  `MarkStarted()` is called when boot actually completes.
- **The handlers are O(1).** Checks run on a background prober and publish an
  immutable snapshot through an `atomic.Pointer`; the handler reads it. A probe
  handler that took the storage lock would turn one slow disk into a fleet-wide
  readiness failure — and when the kubelet's own timeout fired, the handler's
  goroutine would still be holding that lock.
- **The one registered check is `Storage.Ping`.** For the TSDB it reports the last
  *background fsync error*, not a fresh fsync. That is the failure this system can
  otherwise hide indefinitely: `Write` returns nil (the bytes are in the page
  cache), the head serves them back on query, and the fsync has been failing for
  an hour on a full disk. Nothing in the write path notices. `syncLoop` used to log
  the error and drop it; now it publishes it, and the replica leaves the load
  balancer instead of accepting writes it cannot keep.

### 5.2 Shutdown, in the only order that works

`http.Server.Shutdown` closes the listener at once, then waits for in-flight
requests. What it cannot do is tell the load balancer. Kubernetes dispatches
SIGTERM and the endpoint removal *concurrently*, and the removal takes a moment to
reach every kube-proxy and every ingress. A server that closes its listener on the
first instant of SIGTERM spends that moment refusing connections that are still
being routed to it — one burst of 502s per pod, on every deploy.

So `cmd/server` runs three contexts, and `awaitShutdown` orders them:

1. SIGTERM arrives. `health.SetReady(false)` — `/readyz` starts answering 503 with
   its checks still reporting `ok`, which is how an operator tells *draining* from
   *broken*.
2. Wait `-shutdown-delay` (0 locally; ~5s behind a load balancer). The API keeps
   serving. This is not a guess at how long requests take — `Shutdown` already
   waits for those — it is a guess at how long the cluster takes to notice.
3. Cancel `runCtx`: HTTP, gRPC and alerting drain.
4. `pipeline.Shutdown()`, then `store.Close()` (flush the WAL, release the lock).
5. Cancel `telCtx` last, so the probes answer throughout and the final scrape
   before the pod disappears is not a connection refused.

`-shutdown-timeout` (25s) is a hard deadline on steps 3–4, deliberately under
Kubernetes' default 30s grace period. The chart derives
`terminationGracePeriodSeconds` from it, and a test asserts
`delay < timeout < gracePeriod`.

### 5.3 `/metrics`, written from scratch (`internal/promexport`)

The Prometheus text exposition format (v0.0.4) is an encoding, and this project
writes its own encodings. `internal/server/storage/serieskey.go` is the precedent:
an encoding is only safe if it is unambiguous, and the way to know is to invert it.
`FuzzWriteRoundTrip` is the same proof `ParseSeriesKey` was.

Three details are the whole job:

- **HELP escaping and label-value escaping differ.** HELP escapes `\` and newline.
  A label value also escapes `"`. A metric name may contain `:`; a label name may
  not.
- **Histograms, not summaries.** Buckets are cumulative and `le="+Inf"` equals
  `_count`. A summary computes its quantiles inside one process, and there is no
  way to average a p99 across replicas; buckets add. `histogram_quantile` runs at
  query time, in Prometheus.
- **Every label is bounded.** `route` is the mux's registered pattern (obtained
  from `http.ServeMux.Handler`, not from `r.URL.Path`), and an unmatched request
  is folded into `route="other"`. Both vectors carry a hard cardinality cap with a
  visible `__overflow__` series: a silent drop hides the fact that the labelling is
  wrong. A `/metrics` endpoint whose cardinality is driven by request input is a
  memory leak with a scrape interval.

Runtime metrics come from `runtime/metrics`, not `runtime.ReadMemStats`, which
stops the world for the duration of the read. They are namespaced
`traceforge_go_*` rather than borrowing `client_golang`'s `go_*` names, because a
community dashboard panel that silently means something slightly different is worse
than one that shows no data.

---

## 5b. Containers and Kubernetes (`deploy/`)

### 5b.1 GOMAXPROCS, GOMEMLIMIT, and the advice that is now wrong

`internal/container` sets `GOMEMLIMIT` from the cgroup and deliberately does **not**
set `GOMAXPROCS`.

Since Go 1.25 the runtime derives the default `GOMAXPROCS` on Linux from the cgroup
CPU quota, and *re-derives it periodically* as the quota changes. The formula is not
a plain minimum, and the difference bites in exactly the container this exists for
(`runtime/cgroup_linux.go`, `adjustCgroupGOMAXPROCS`):

```text
GOMAXPROCS = min(CPUs from sched_getaffinity, max(ceil(quota/period), 2))
```

A fractional quota rounds up — `--cpus=1.5` gives 2 — and a sub-two-core quota is
floored at 2 unless the affinity mask allows fewer. `--cpus=1` on an eight-core host
gives 2, not 1.

Setting the `GOMAXPROCS` environment variable, which almost every Helm chart does
through the downward API, **disables that updating**. So does calling
`runtime.GOMAXPROCS(n)` with any positive `n`, including the innocent-looking
`runtime.GOMAXPROCS(runtime.NumCPU())`. (`runtime.GOMAXPROCS(0)` is a safe read: the
runtime returns before it marks the value custom. This package reads
`/sched/gomaxprocs:threads` from `runtime/metrics` instead, so the dangerous call is
not merely discouraged but unrepresentable.)

`GOMEMLIMIT` is the opposite case: the runtime does not derive it from anything.
Left unset, the heap grows until the cgroup's hard limit and the kernel OOM-kills a
process that would have run a GC instead. So the binary reads `memory.max` — walking
the cgroup v2 chain to the root and taking the minimum, because a parent's limit
binds its children whatever the leaf says — and calls `debug.SetMemoryLimit`.

The ratio (`-memory-limit-ratio`, default 0.9) is a knob and not a constant because
**`GOMEMLIMIT` bounds only what the Go runtime manages.** It does not bound the
binary's text, allocations made by cgo, or file pages brought in by `mmap` — and this
project's TSDB mmaps its chunk files. Those pages count against the cgroup and are
invisible to `GOMEMLIMIT`. The headroom below 1.0 is where they live.

The same reasoning found a real bug: the pipeline sized its worker pools from
`runtime.NumCPU()`, which reports the machine's cores and knows nothing about a
quota. On an 8-core host with `--cpus=1.5` that started sixteen goroutines to share
two Ps. They are sized from `GOMAXPROCS` now.

### 5b.2 The images

One `deploy/docker/Dockerfile`, four targets.

| Target | Base | User | Size | Notes |
|--------|------|------|------|-------|
| `server` | `distroless/static:nonroot` | 65532 | ~25 MB | static, `CGO_ENABLED=0` |
| `agent` | `distroless/static:nonroot` | 65532 | ~23 MB | no packet capture |
| `agent-pcap` | `distroless/static` | **0** | ~24 MB | cgo, musl-static, libpcap 1.10.6 |
| `metricsctl` | `distroless/static:nonroot` | 65532 | ~16 MB | |

- The builder stage is `FROM --platform=$BUILDPLATFORM golang:1.26-alpine`. Without
  it, a multi-platform build fetches the *target*'s Go image and runs the compiler
  under QEMU — minutes instead of seconds, to cross-compile a language that
  cross-compiles for free.
- `agent-pcap` is the one stage that cannot do that: cgo needs a C toolchain and
  libpcap's headers for the *target*. It builds natively, under emulation when the
  architectures differ. That is the tax CGo levies, and `make cross-cgo` exists to
  fail on purpose.
- It runs as **root**, and it is a separate image for that reason. Opening a raw
  socket needs `CAP_NET_RAW`, and a capability granted by the container runtime
  lands in the permitted set of a *root* process; a non-root process needs file or
  ambient capabilities, which distroless does not provide. The chart selects this
  image only when `agent.capture.enabled` is set.
- distroless has no shell, so a shell-form `HEALTHCHECK` cannot run. The exec form
  can, given something in the image that speaks HTTP — and the binary already does:
  `server -health-check` GETs its own `/readyz` and exits 0 or 1.
- A fresh named volume inherits the ownership of the image's directory at its mount
  point. The runtime stage has no shell to `mkdir` with, so the data directory is
  created in the builder stage and `COPY --chown=65532:65532`'d across. Kubernetes
  solves the same problem with `fsGroup`, which is why the chart sets it.

### 5b.3 The chart is the source; the manifests are generated

`deploy/charts/traceforge` is the only hand-written Kubernetes YAML.
`deploy/k8s/traceforge.yaml` is rendered from it by `make manifests` and committed,
so an operator without helm can `kubectl apply -f`, and so the Go tests in
`test/deploy` can assert on the exact bytes a cluster receives. CI runs
`make manifests-check` and fails on a diff.

`values-prod.yaml` pins `server.replicas: 1`. Two replicas would be two independent
stores with no replication between them — data loss dressed as scale-out. Horizontal
scaling waits for clustering.

### 5b.4 Deployment artefacts are source code with no compiler

Nothing compiles a manifest, executes a dashboard, or type-checks an alert. So
`test/deploy` is their compiler:

- every `-flag` in a manifest is cross-checked against the flag names parsed out of
  `cmd/server` and `cmd/agent` with `go/ast` — a container started with an unknown
  flag exits 2 with a usage message nobody is tailing;
- every `traceforge_*` token in every dashboard panel and every alert expression must
  be a metric some binary really exports — a misspelled metric renders an empty graph,
  which is indistinguishable from a healthy system;
- every alert's `runbook_url` must name a file that exists, and every runbook must be
  named by exactly one alert;
- the rendered manifests must be hardened (`runAsNonRoot`, `readOnlyRootFilesystem`,
  `drop: [ALL]`, `seccompProfile: RuntimeDefault`, resources on both sides, no
  `:latest`, probes on ports the container actually declares);
- `shutdown-delay < shutdown-timeout < terminationGracePeriodSeconds`.

The exposition format has an external oracle: CI pipes a live scrape through
`promtool check metrics`, which is Prometheus's own validator and does not care what
wrote the bytes.

### 5b.5 Supply chain

`govulncheck` walks the call graph, so a vulnerable function nothing calls does not
fail the build. That precision is what makes it worth gating on. It found eight
reachable advisories at the start of this stage — six in the standard library, fixed
by `toolchain go1.26.5`, and two in `grpc` and `x/net`. All are gone.

`.goreleaser.yaml` builds archives for linux/darwin (and metricsctl for Windows),
generates an SPDX SBOM per archive, and signs the checksum file with **keyless**
Sigstore: no private key exists, the identity comes from the GitHub Actions OIDC
token, and the signature lands in Rekor's public transparency log. Images are built
by `docker buildx` in the release workflow rather than by GoReleaser, because
`agent-pcap` must be compiled on the architecture it runs on.

The release workflow's tag trigger is **commented out**. A tag-triggered release
pushes images to a registry and writes an append-only entry into a public transparency
log, and nobody following along with this repository has consented to either by typing
`git push --tags`.

---

## 6. Package layout

```text
proto/metrics/v1/           protobuf service + messages
cmd/{agent,server,metricsctl}/  entry points + flag/env config
cmd/{benchcmp,mutate}/      dev tools: significance-tested benchstat, mutation tester
internal/
  model/                    Metric, Batch, MetricType
  agent/                    collectors, HTTP + gRPC senders, orchestration
    network/                libpcap binding (CGo) + pure-Go packet parser
    kernel/                 /proc/net counters — the same metrics, no CGo
  auth/                     principal/RBAC, API keys, JWT (HS256/RS256), JWKS
  buildinfo/                version/commit/dirty: ldflags reconciled with VCS
  container/                cgroup memory limit -> GOMEMLIMIT (GOMAXPROCS left alone)
  promexport/               from-scratch Prometheus text exposition + counter/gauge/histogram
  telemetry/                the admin listener: probes + /metrics + SelfCheck
  benchcmp/                 go test -bench parser + Mann-Whitney U test
  mutate/                   AST mutators + go test -overlay runner
  clock/                    injectable time: Real + deterministic Fake
  testutil/                 builders, assertions, Eventually, goroutine-leak detector
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
    health/                 liveness/readiness/startup + background prober
examples/alerting/          sample rules + receivers configuration
web/                        embedded dashboard SPA (go:embed)
pkg/httpx/                  reusable HTTP client (retry + backoff)
deploy/
  docker/Dockerfile         4 targets: server, agent, agent-pcap (cgo), metricsctl
  compose.yaml              server + agent, and prometheus/grafana behind a profile
  charts/traceforge/        the Helm chart: the only hand-written Kubernetes YAML
  k8s/traceforge.yaml       rendered from the chart by `make manifests`, drift-checked
  prometheus/               scrape config + 10 alert rules, each linking a runbook
  dashboards/               3 Grafana dashboards, metric names checked by a test
docs/runbooks/              one per alert; structure enforced by a test
test/e2e/                   //go:build e2e: real binaries as separate processes
test/deploy/                the compiler the deployment artefacts otherwise lack
```

---

## 4c. Testing architecture (`internal/testutil`, `test/e2e`, build tags)

Tests are a pyramid, and the layers are kept apart by build tag so the fast base
stays fast:

```text
              ┌─────────────┐
              │    e2e      │  //go:build e2e — real server/agent/metricsctl
              │  (seconds)  │  processes on :0 ports; addr read from the log
              ├─────────────┤
              │ integration │  //go:build integration — real bbolt/tsdb files,
              │  (< 1s)     │  httptest, gRPC over bufconn, crash recovery
              ├─────────────┤
              │    unit     │  no tag, -race — pure logic, t.TempDir at most,
              │  (ms each)  │  the wide base run on every push
              └─────────────┘
```

- **`internal/testutil`** is the shared kit, imported only from `_test.go`:
  object builders (so a new `Metric` field changes one line, not two hundred),
  custom assertions, an `Eventually`/`Never` poller for the pipeline's async
  boundary, golden-file support with `-update`, and a **goroutine-leak detector**
  that diffs the set of live goroutine IDs across a test and reports any that
  outlived it — the standard way a Go service dies slowly.
- **Fuzzing** targets the code that parses untrusted bytes and asserts
  *invariants*: `SeriesKey`↔`ParseSeriesKey` round-trip and injectivity, the rule
  parser's `Parse`↔`String` idempotency, the chunk codec's point conservation,
  WAL replay's prefix property, JWT forgery-resistance. Crashers are committed
  under `testdata/fuzz/` and then run by every plain `go test`.
- **`benchcmp` and `mutate`** are first-class parts of the architecture, not
  scripts: benchmarks feed `benchcmp`'s significance test, and `mutate` grades the
  test suite itself by editing one operator and rerunning the tests through a
  `go test -overlay`.

The e2e harness (`test/e2e/harness_test.go`) builds the three binaries once in
`TestMain`, starts each with `-addr=127.0.0.1:0`, and recovers the
kernel-assigned port from the server's structured startup log — which is why the
lifecycle logs the addresses it actually bound. That is what lets every e2e test
run its own server concurrently with no port registry.

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
- **The identity of a series must be injective.** `SeriesKey` and
  `alert.Fingerprint` both encode a name plus a label set into one string, and
  both must be reversible — a collision silently merges two series or two alerts.
  Both escape (or length-prefix) their delimiters; the round-trip decoder is the
  proof, and a fuzzer guards it.
- **Tests are code, and graded like it.** Coverage says a line ran; the shipped
  `mutate` tool says whether a test would notice if that line were wrong. Fuzzers
  assert invariants, not just the absence of a panic. This is why several stage-9
  fixes are for bugs that lived under a green suite.

---

## 8. What's next

Single-node, and now deployable. The staged plan is complete: the testing,
benchmarking and profiling deep-dive, the CGo boundary, and a production
deployment have all landed.

What is deliberately absent, and what it would take:

- **Clustering.** `values-prod.yaml` pins one replica because two would be two
  independent stores. Gossip membership, consistent-hash sharding of series across
  nodes, and Raft for the alerting state would make `replicas: 3` mean something.
  Everything in the chart that says "single-node" is a marker for that work.
- **Distributed tracing.** The metrics and the logs both carry a request id; a
  trace would carry it across the agent→server→storage boundary. The interesting
  part is W3C trace-context propagation and sampling, not the exporter — and an
  exporter with no collector to send to would be code nobody has run, which this
  project does not ship.
- **Persistent alerting state.** The rule, state and silence stores are in-memory:
  a restart forgets pending retries and silences, and re-arms `for` windows.

Known limitations of the alerting subsystem, deliberately deferred: the rule,
state and silence stores are in-memory (a restart forgets pending retries and
silences, and re-arms `for` windows), and there is no escalation, acknowledgement
or recurring maintenance window.
