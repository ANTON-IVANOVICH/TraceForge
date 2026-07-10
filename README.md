# TraceForge

A distributed metrics collection and analysis system in Go (a simplified
Prometheus-like stack). The Go module is `metrics-system`; it ships three
binaries — an **agent** that collects host metrics, a **server** that ingests,
stores, serves and alerts on them, and **metricsctl**, a command-line client.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the internals and
[SCENARIOS.md](SCENARIOS.md) for end-to-end usage flows.

## Architecture

```text
              HTTP  POST /api/v1/metrics ──┐
agent(s) ─────┤                            ├──► server
              gRPC  IngestStream (stream) ─┘      │
                                                  ▼
           ingest ─► validate ─► enrich ─► store   (channel pipeline, worker pools)
                                                  │
                                                  ▼
                     storage: memory | bolt | tsdb ──► alerting ──► receivers
                                                  ▲    (rules, for,   (log, webhook,
              HTTP  GET /api/v1/query ────────────┤     grouping)      slack, email)
              gRPC  Query (server stream) ────────┘
```

- **Agent** — every N seconds collects CPU, RAM, disk and uptime, packs them
  into a `Batch` and ships it over **HTTP** (JSON) or **gRPC** (protobuf,
  streaming), selectable with `-transport`.
- **Server** — exposes both an HTTP and a gRPC transport that feed the *same*
  channel pipeline (`ingest → validate → enrich → store`), with configurable
  worker pools and backpressure (a full ingest buffer returns HTTP `503` /
  gRPC `ResourceExhausted`). Metrics land in a persistent-or-in-memory
  time-series store (one series per name+labels, indexed by name) and are read
  back through a query API with label filtering and aggregations.

## Layout

```text
.
├── proto/metrics/v1/          # protobuf service + message definitions
├── cmd/
│   ├── agent/main.go          # agent entry point (transport selection)
│   ├── server/main.go         # server entry point + config (HTTP + gRPC)
│   └── metricsctl/main.go     # CLI entry point (signals, exit codes)
├── internal/
│   ├── agent/                 # collectors, HTTP sender, gRPC streaming sender
│   ├── model/metric.go        # Metric, Batch, MetricType
│   ├── auth/                  # API keys, JWT (HS256/RS256+JWKS), RBAC, tenant principal
│   ├── clock/                 # injectable time (Real + Fake) for deterministic tests
│   ├── grpcconv/              # model <-> protobuf conversion
│   ├── proto/metricspb/       # generated protobuf + gRPC code (go generate)
│   ├── cli/                   # metricsctl: cobra command tree, contexts, printers
│   │   ├── config/            # ~/.metricsctl/config.yaml: contexts + credentials
│   │   ├── client/            # HTTP client for the server API
│   │   └── output/            # table/json/yaml/name printers, TTY + NO_COLOR
│   ├── alerting/
│   │   ├── service.go         # assembly + tenant-scoped rules/alerts/silences API
│   │   ├── rules/             # PromQL-lite DSL (lexer, parser, AST), evaluator, scheduler
│   │   ├── alert/             # alert model, grouping, dedup
│   │   ├── silence/           # silences + label matchers
│   │   ├── inhibit/           # alert suppression rules
│   │   ├── notify/            # dispatcher, retry queue, circuit breaker, receivers
│   │   └── config/            # receivers/routing + bootstrap rules loading
│   └── server/
│       ├── handler.go         # thin HTTP handlers (ingest, query, stats, ui, pprof)
│       ├── alerthandler.go    # rules / alerts / silences API
│       ├── middleware.go      # recover, request id, logging, rate limiting
│       ├── authmw.go          # auth + RBAC middleware
│       ├── server.go          # http.Server + graceful lifecycle
│       ├── grpcserver/        # gRPC service, interceptors, lifecycle
│       ├── pipeline/          # channel pipeline: stages, worker pools, self-stats
│       ├── storage/           # TSDB: series/index/query + memory, bolt, tsdb backends
│       ├── live/              # from-scratch WebSocket + broadcast hub (live dashboard)
│       └── ratelimit/         # per-agent token-bucket limiter
├── examples/alerting/         # sample rules + receivers configuration
├── web/                       # embedded dashboard SPA (go:embed)
└── pkg/
    └── httpx/client.go        # reusable HTTP client with retry + backoff
```

## Build & run

```bash
make tidy
make build          # -> bin/server, bin/agent, bin/metricsctl
make test           # go test -race -cover ./...
```

Terminal 1 — server:

```bash
make run-server
```

Terminal 2 — agent:

```bash
make run-agent
```

## HTTP API

- `POST /api/v1/metrics` — ingest a batch → `202 Accepted` (or `503` when overloaded)
- `GET  /api/v1/query` — query stored metrics (see parameters below)
- `GET  /debug/stats` — pipeline + storage self-metrics (JSON)
- `GET  /healthz` — liveness check
- `GET  /debug/pprof/` — runtime profiling (`net/http/pprof`)

With `-alerting` (all tenant-scoped; reads need `query`, writes need `admin`):

- `GET|POST /api/v1/rules`, `GET|PUT|DELETE /api/v1/rules/{id}` — manage rules
- `POST /api/v1/rules/preview` — backtest an expression without saving it
- `GET  /api/v1/alerts` — currently pending and firing alerts
- `GET|POST /api/v1/silences`, `DELETE /api/v1/silences/{id}` — manage silences

### Query parameters (`GET /api/v1/query`)

- `name` — metric name (**required**)
- any other key — treated as a label filter, e.g. `host=web-1`, `agent_id=a1`
- `from`, `to` — RFC3339 time bounds (open if omitted)
- `agg` — `avg` | `min` | `max` | `sum` | `count` | `p50` | `p90` | `p95` | `p99`
- `step` — aggregation window, e.g. `1m` (requires `agg`)
- `limit` — max points returned

### Examples

```bash
# Ingest a batch
curl -i -XPOST http://localhost:8080/api/v1/metrics \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"demo","metrics":[{"name":"cpu_usage_percent","type":"gauge","value":12.5,"timestamp":"2026-07-04T10:00:00Z"}]}'

# Raw points
curl -s 'http://localhost:8080/api/v1/query?name=cpu_usage_percent' | jq

# Filter by label + 1-minute average
curl -s 'http://localhost:8080/api/v1/query?name=memory_used_percent&host=my-host&agg=avg&step=1m' | jq

# Pipeline/storage stats and health
curl -s http://localhost:8080/debug/stats | jq
curl -i http://localhost:8080/healthz
```

## gRPC transport

Alongside HTTP, the server speaks gRPC (Protocol Buffers) on a separate port
(`-grpc-addr`, default `:9090`), feeding the *same* pipeline and store. The
service (`proto/metrics/v1/metrics.proto`) offers all three RPC styles:

- `Ingest(Batch) → IngestAck` — **unary**; backpressure surfaces as
  `ResourceExhausted`.
- `IngestStream(stream Batch) → stream IngestAck` — **bidirectional**; the agent
  keeps one long-lived stream open and the server acks each batch, carrying a
  `throttled` flag back so a fast client can back off without a new RPC per tick.
- `Query(QueryRequest) → stream Metric` — **server streaming**.

Point the agent at it with `-transport=grpc`:

```bash
./bin/server                                   # HTTP :8080 + gRPC :9090
./bin/agent -transport=grpc -grpc-server=localhost:9090
```

Server reflection is enabled, so you can poke it with
[`grpcurl`](https://github.com/fullstorydev/grpcurl):

```bash
grpcurl -plaintext localhost:9090 list metrics.v1.MetricsService
grpcurl -plaintext -d '{"name":"cpu_usage_percent"}' \
  localhost:9090 metrics.v1.MetricsService/Query
```

Regenerate the protobuf/gRPC code after editing the `.proto` with
`make proto-tools` (once) then `make proto` (or `go generate ./...`).

## Live dashboard

The server embeds a single-page dashboard (via `go:embed`) at `/` and pushes
live updates over a WebSocket implemented from scratch (RFC 6455 — no
third-party library). Enabled by default; disable with `-ui=false`.

```bash
./bin/server                 # open http://localhost:8080/
./bin/agent                  # feed it some metrics
```

The dashboard shows a live metric feed and (for admin/no-auth) the pipeline &
storage counters with a small chart. When auth is on, the WebSocket takes the
credential from a field in the page (sent as a query param, since browsers can't
set headers on a WS handshake) and the live stream is **tenant-scoped** — a
tenant only ever sees its own metrics, and stats are admin-only.

## Authentication & multi-tenancy

Auth is **off by default** (single-tenant, open). Enable it with `-auth` plus at
least one credential source; it then guards both the HTTP and gRPC transports.

**Credentials** (the agent presents one; the server accepts any configured kind):

- **API keys** — an `X-API-Key` header / `x-api-key` gRPC metadata. Keys map to a
  `{subject, tenant, roles}` identity in a JSON file (`-api-keys`); keys are
  stored only as SHA-256 hashes.
- **JWT bearer** — `Authorization: Bearer <token>`. Verified from scratch on the
  standard library, pinned to one algorithm: **HS256** (`-jwt-hs256-secret`) or
  **RS256** via a rotating **JWKS** (`-jwks-url`). `exp` is mandatory; `-jwt-issuer`
  / `-jwt-audience` are enforced when set. `tenant` and `roles`/`scope` claims map
  to the principal.

**RBAC** — roles grant actions: `writer`→ingest, `reader`→query, `admin`→all
(incl. `/debug/*`). A missing credential is `401`/`Unauthenticated`; a valid one
lacking the role is `403`/`PermissionDenied`.

**Multi-tenancy** — the server stamps each ingested metric with a server-side
`tenant` label taken from the authenticated principal (clients cannot set or
spoof it), and forces `tenant=<caller>` onto every query, so a tenant can only
ever read its own series.

```bash
# api-keys.json: [{"key":"K1","subject":"web","tenant":"acme","roles":["writer"]}, ...]
./bin/server -auth -api-keys=./api-keys.json -jwt-hs256-secret=$SECRET
./bin/agent -api-key=K1                    # HTTP
./bin/agent -transport=grpc -api-key=K1    # gRPC
curl -H 'X-API-Key: K2' 'localhost:8080/api/v1/query?name=cpu_usage_percent'
```

## Alerting

Off by default; enable with `-alerting`. Rule evaluation and notification
delivery are deliberately separated: evaluation is periodic and deterministic,
delivery is slow and talks to services that fail. They meet over one channel.

```text
rules ──► evaluator ──► alerts ──► grouper ──► notifier ──► receivers
 (DSL)   (for/state)   (channel)  (dedup)    (breaker +    (log, webhook,
                                              retry queue)   slack, email)
                       silences ──┘  inhibitions ──┘
```

**Rule DSL** — a PromQL-lite language with a hand-written lexer and
recursive-descent parser (`internal/alerting/rules`):

```text
cpu_usage_percent > 90 for 5m
avg_over_time(memory_used_percent[5m]) > 95
max by (agent_id) (disk_used_percent) > 85
rate(http_requests_total[1m]) > 1000 and up{env=~"prod.*"} == 1
```

Comparison **filters**: `cpu > 90` evaluates to the samples that breach it.
Functions: `rate`, `increase`, `delta`, `{avg,min,max,sum,count,last,stddev}_over_time`,
`abs`, `ceil`, `floor`, `round`, `clamp_min`, `clamp_max`; aggregations
`sum|avg|min|max|count|stddev` with `by`/`without`; operators `and`, `or`,
`unless`, arithmetic, and `=`,`!=`,`=~`,`!~` label matchers (regexes anchored).

**`for` semantics** — the condition must hold *continuously*. An alert walks
`inactive → pending → firing → resolved`; a resolution is always announced. Its
identity is a stable fingerprint over the rule ID plus sorted labels, so a
re-evaluation is a dedup, not a new page.

**Delivery** — alerts are grouped by `group_by` labels (one notification for
fifty failed hosts), muted by **silences**, suppressed by **inhibition rules**
(`HostDown` on a host hides `CPUHigh` on it), then delivered per receiver with
exponential backoff **plus jitter** and a **circuit breaker** per receiver.
Webhooks are HMAC-signed (`X-TraceForge-Signature`, timestamped against replay).

```bash
./bin/server -alerting \
  -alert-rules=./examples/alerting/rules.json \
  -alert-config=./examples/alerting/receivers.json
```

Rules, alerts and silences are managed at runtime (hot reload, no restart):

```bash
curl localhost:8080/api/v1/alerts
curl -XPOST localhost:8080/api/v1/rules -d '{"name":"CPUHigh","expression":"cpu_usage_percent > 90 for 2m","receivers":["log"]}'
curl -XPOST localhost:8080/api/v1/rules/preview -d '{"expression":"cpu_usage_percent > 90","step":"1m"}'   # backtest
curl -XPOST localhost:8080/api/v1/silences -d '{"matchers":[{"name":"agent_id","op":"=","value":"web-1"}],"duration":"2h"}'
```

With `-auth` on, everything is tenant-scoped: a rule evaluates only against its
own tenant's series, and a tenant can never see another's rules, alerts or
silences. Reads need the `query` action; creating or deleting needs `admin`.

## CLI — `metricsctl`

A kubectl-shaped client for the server. The shape is borrowed on purpose:
`noun verb`, persistent flags, `-o json`, named contexts, shell completion.

```bash
make build            # -> bin/metricsctl
make install-ctl      # or: go install ./cmd/metricsctl
```

**Contexts.** One binary addresses production and staging without editing
anything in between. The config lives at `~/.metricsctl/config.yaml` (or
`$XDG_CONFIG_HOME/metricsctl/`, or `$METRICSCTL_CONFIG`) and is written `0600`,
because it holds credentials.

```bash
metricsctl config set-context local --server http://localhost:8080 --use
metricsctl config set-context prod  --server https://metrics.example.com \
                                    --token-file ~/.metricsctl/prod.token
metricsctl config get-contexts
metricsctl --context prod alerts list      # one-off, without switching
```

**Everything composes.** `-o table` for humans, `-o json` for `jq`, `-o yaml` to
paste into a file, `-o name` for `xargs`:

```bash
metricsctl query cpu_usage_percent -l agent_id=web-1 --from -1h --agg avg --step 1m
metricsctl alerts list --watch                     # refresh until Ctrl+C
metricsctl alerts list -o json | jq '.[] | select(.state=="firing") | .labels'
metricsctl rules list -o name | xargs -n1 metricsctl rules get -o yaml
metricsctl agents list
metricsctl stats
```

**Declarative rules.** `apply` is idempotent — `metadata.name` is the rule's
stable id, so re-applying an unchanged file writes nothing:

```bash
metricsctl rules apply -f examples/alerting/rules.yaml --dry-run
metricsctl rules apply -f examples/alerting/rules.yaml
cat rules.yaml | metricsctl rules apply -f -
metricsctl rules preview 'cpu_usage_percent > 80' --from -1h --step 1m   # backtest
```

**Silences.**

```bash
metricsctl silences create -m agent_id=web-1 --duration 2h --comment "maintenance"
metricsctl silences list
metricsctl silences delete <id> --yes
```

**Exit codes are the contract**, so `metricsctl rules get foo || handle_missing`
works: `0` success, `1` generic failure, `2` usage error, `3` authentication or
authorization, `4` not found.

**Terminal manners.** Colour only on a real terminal, never in a pipe or a file,
and `NO_COLOR` (see [no-color.org](https://no-color.org)) or `--no-color` always
wins. A destructive command refuses to proceed unattended without `--yes`.

**Shell completion** is dynamic — `metricsctl rules get <TAB>` asks the server
which rules exist:

```bash
source <(metricsctl completion bash)     # or zsh|fish|powershell
```

## Network metrics & CGo

The agent can capture packets through **libpcap** and report per-protocol
counters. It is the project's one crossing into C, and it is opt-in:

```bash
sudo ./bin/agent -network -network-device=en0        # live: needs root (/dev/bpf*) or CAP_NET_RAW
./bin/agent -network -network-file=capture.pcap      # a savefile: no privileges at all
./bin/agent -network -network-device=eth0 -network-filter='tcp port 443'
```

The BPF filter is compiled and applied **in the kernel**, so packets that do not
match never cross into user space. `-network-snaplen` caps how much of each
packet is copied out; the headers are all that gets classified.

Metrics: `net_packets_total`, `net_bytes_total` (wire length, not the truncated
copy), `net_protocol_packets_total{protocol=tcp|udp|icmp|other}`,
`net_ip_packets_total{version=4|6}`, `net_unparsed_packets_total`, and
`net_kernel_dropped_total` — which keeps the others honest, because under load
the kernel drops packets before this process ever sees them.

**Every failure is non-fatal.** Built without CGo, no libpcap, no permission, no
such interface: the agent logs once and keeps reporting CPU, memory and disk.

### The cost of CGo

Capture lives behind the `cgo` build tag, because taking a CGo dependency costs
the one-command cross-compile, the static binary, and the ability to build on a
machine without libpcap:

```bash
make build           # with capture (needs libpcap)
make build-nocgo     # CGO_ENABLED=0: complete agent, capture reports unavailable
make cross-nocgo     # cross-compiles to linux/{amd64,arm64}, darwin, windows — from nothing
make cross-cgo       # fails on purpose; read the error
make test-nocgo      # the suite with the C compiler taken away
```

Escape hatches for a CGo cross-build, in order of how much you will regret them:
`docker buildx --platform linux/arm64`, `CC="zig cc -target aarch64-linux-musl"`,
or wanting it less (`CGO_ENABLED=0`).

Boundary costs, measured (`go test -bench . ./internal/agent/network/`):

| | ns/op |
| --- | --- |
| Go call | 0.29 |
| **CGo call** | **20.7** |
| `C.GoBytes` copy (64 B) | 19.6 |
| `C.CString` + `C.free` | 63.9 |
| pass a Go pointer (64 B) | 26.4 |
| ...wrapped in `runtime.Pinner` | 57.7 |
| ...copied with `C.CBytes` | 95.4 |

A CGo call is ~70× a Go call, so a binding must cross **rarely** and do much on
the far side — which is why the C shim returns a whole packet per call, not a
field per call. `runtime.Pinner` is *not* required to pass a `[]byte` to a
synchronous C function (the pointer rules already allow it); pinning anyway
doubles the cost.

### Kernel counters without CGo

Before reaching for CGo — or eBPF — check whether the kernel already publishes
what you want. `internal/agent/kernel` reads TCP retransmits, resets,
listen-queue overflows and UDP errors straight out of `/proc/net/snmp` and
`/proc/net/netstat`: no C, no dependencies, no privileges, and it cross-compiles.
See `ARCHITECTURE.md` for when eBPF earns its price and why it is documented here
rather than shipped.

## Storage backends

Pick the store with `-storage`; persistent backends keep data in `-data-dir`.
All three implement the same `Storage` interface, so the pipeline and API are
identical regardless of choice.

- **`memory`** (default) — fast, in-process; lost on restart.
- **`bolt`** — a persistent bbolt (B+tree) file. Timestamps are big-endian keys,
  so a time-range query is a cursor range scan.
- **`tsdb`** — a from-scratch on-disk engine: a CRC-checked write-ahead log
  (crash recovery), an in-memory head, and immutable chunks written atomically
  and read back via `mmap`. Single-writer (file-locked).

Both persistent backends survive a restart:

```bash
./bin/server -storage=tsdb -data-dir=./data   # ingest metrics, then Ctrl+C
./bin/server -storage=tsdb -data-dir=./data   # restart: the data is still queryable
```

## Configuration (flags + env)

Priority: defaults → environment variables → flags.

### Server

- `-addr` / `SERVER_ADDR` — HTTP listen address (default `:8080`)
- `-grpc-addr` / `GRPC_ADDR` — gRPC listen address, empty to disable (default `:9090`)
- `-log-level` / `SERVER_LOG_LEVEL` — `debug|info|warn|error` (default `info`)
- `-storage` / `STORAGE` — backend: `memory` | `bolt` | `tsdb` (default `memory`)
- `-data-dir` / `DATA_DIR` — data directory for the `bolt`/`tsdb` backends (default `./data`)
- `-ingest-buffer` / `INGEST_BUFFER` — ingest channel buffer / backpressure (default `1000`)
- `-validate-workers` / `VALIDATE_WORKERS` — validate stage workers (default `NumCPU`)
- `-enrich-workers` / `ENRICH_WORKERS` — enrich stage workers (default `NumCPU`)
- `-store-workers` / `STORE_WORKERS` — store stage workers (default `1`)
- `-rate-limit-rps` / `RATE_LIMIT_RPS` — per-agent requests/second (default `100`)
- `-rate-limit-burst` / `RATE_LIMIT_BURST` — per-agent burst (default `200`)
- `-auth` / `AUTH` — enable auth + RBAC + tenant isolation (default `false`)
- `-api-keys` / `API_KEYS_FILE` — path to the API-keys JSON file
- `-jwt-hs256-secret` / `JWT_HS256_SECRET` — HS256 shared secret for JWT auth
- `-jwks-url` / `JWKS_URL` — JWKS endpoint for RS256 JWT auth
- `-jwt-issuer` / `JWT_ISSUER`, `-jwt-audience` / `JWT_AUDIENCE` — required claims (optional)
- `-ui` / `UI` — serve the embedded live dashboard at `/` (default `true`)
- `-alerting` / `ALERTING` — enable rule evaluation and notifications (default `false`)
- `-alert-rules` / `ALERT_RULES_FILE` — bootstrap rules JSON file
- `-alert-config` / `ALERT_CONFIG_FILE` — receivers/routing/inhibition JSON file
- `-alert-lookback` / `ALERT_LOOKBACK` — how far back an instant selector reaches (default `5m`)
- `-alert-buffer` / `ALERT_BUFFER` — evaluator→notifier channel buffer (default `1024`)

### Agent

- `-transport` / `AGENT_TRANSPORT` — `http` | `grpc` (default `http`)
- `-server` / `AGENT_SERVER` — HTTP endpoint (default `http://localhost:8080/api/v1/metrics`)
- `-grpc-server` / `AGENT_GRPC_SERVER` — gRPC target host:port (default `localhost:9090`)
- `-api-key` / `AGENT_API_KEY` — API key to authenticate to the server
- `-auth-token` / `AGENT_AUTH_TOKEN` — bearer (JWT) token to authenticate to the server
- `-interval` / `AGENT_INTERVAL` — default `5s`
- `-id` / `AGENT_ID` — default hostname
- `-disk-path` / `AGENT_DISK_PATH` — default `/`
- `-http-timeout` / `AGENT_HTTP_TIMEOUT` — default `10s`
- `-http-retries` / `AGENT_HTTP_RETRIES` — default `2`
- `-http-backoff` / `AGENT_HTTP_BACKOFF` — default `200ms`
- `-log-level` / `AGENT_LOG_LEVEL` — default `info`

## Testing & profiling

The test suite is a pyramid: a wide base of fast unit tests, a middle of
integration tests against real dependencies, and a thin top of end-to-end tests
that run the actual binaries. Each layer is a separate `make` target and a
separate build tag, so the fast layer stays fast.

```bash
make test              # unit: -race, no build tag, milliseconds each
make test-integration  # //go:build integration: real bbolt/tsdb files, httptest, bufconn
make test-e2e          # //go:build e2e: builds the binaries, runs them as processes
make test-all          # all three
make cover             # -covermode=atomic coverage; open with `make cover-html`
```

**Fuzzing.** Seventeen fuzz targets cover the parsers and codecs that touch
untrusted bytes — the rule DSL, the WAL replay, the chunk reader, the JWT
verifier, protobuf and JSON decoding, and the series-key encoding. They assert
*invariants* (round-trip, injectivity, conservation), not merely "does not
panic". A plain `go test` replays every committed corpus entry; `make fuzz`
looks for new inputs.

```bash
make fuzz                                # 15s per target
make fuzz FUZZTIME=10m                    # the nightly depth
```

**Benchmarks and `benchcmp`.** `make bench` runs the suite; `benchcmp`
(`cmd/benchcmp`, written here rather than pulled in) reports whether a change is
*statistically significant* using a Mann-Whitney U test, so noise reads as `~`
rather than a phantom regression.

```bash
git switch main    && make bench-save NAME=base
git switch mybranch && make bench-save NAME=new
make bench-compare OLD=base NEW=new
```

**Mutation testing.** `make mutate PKG=./internal/alerting/rules` edits one
operator at a time (`>` → `>=`, `&&` → `||`) and reruns the tests; a mutant that
survives is a line the tests execute but never check — a hole coverage cannot
see. The tester (`cmd/mutate`) uses `go test -overlay`, so it never copies the
tree.

**Profiling.** pprof lives on its own listener, off by default, because
`/debug/pprof/cmdline` exposes the process's argv and `/heap` exposes the data:

```bash
./bin/server -pprof-addr=127.0.0.1:6060 &                 # loopback only
make profile-cpu   PROF_PKG=./internal/server/storage      # flame graph from a benchmark
make profile-heap  PROF_PKG=./internal/server/storage      # alloc_space
make profile-trace PROF_PKG=./internal/server/pipeline     # go tool trace: why it won't scale
go tool pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=10'
```

## Milestones

Development is trunk-based on `main`, with a SemVer tag per milestone:

- **v0.1.0** — MVP: agent + in-memory server with a REST API.
- **v0.2.0** — Channel pipeline, in-memory TSDB, query API with aggregations,
  middleware chain, per-agent rate limiting, self-stats and pprof.
- **v0.3.0** — Persistent storage: `bolt` (bbolt B+tree) and a from-scratch
  on-disk `tsdb` (WAL + immutable mmap'd chunks) behind the `Storage` interface,
  selectable with `-storage`; both survive a restart.
- **v0.4.0** — gRPC + Protocol Buffers transport alongside HTTP: unary,
  bidirectional-streaming and server-streaming RPCs feeding the same pipeline;
  agent `-transport=grpc`; recovery/logging interceptors and reflection.
- **v0.5.0** — Authentication & multi-tenancy: API keys and from-scratch JWT
  (HS256/RS256 + JWKS rotation), RBAC, and per-tenant data isolation enforced on
  both transports; auth off by default.
- **v0.6.0** — Embedded live dashboard: a `go:embed` SPA and a from-scratch
  WebSocket pushing tenant-scoped live metrics and (admin) stats.
- **v0.7.0** — Alerting: a PromQL-lite rule DSL (hand-written lexer +
  recursive-descent parser), the `for` alert state machine, grouping with
  dedup, silences and inhibition, receivers (log/webhook/Slack/email) with
  HMAC-signed webhooks, retry with exponential backoff + jitter and a circuit
  breaker per receiver; a tenant-scoped rules/alerts/silences API and an alerts
  panel on the dashboard. Off by default.
- **v0.8.0** — `metricsctl`, a kubectl-shaped CLI on Cobra: named contexts with
  `0600` credentials, `table|json|yaml|name` output, declarative idempotent
  `rules apply -f`, `alerts list --watch`, silences, dynamic shell completion,
  TTY/`NO_COLOR` manners, and POSIX exit codes.
- **v0.10.0** — CGo: a from-scratch **libpcap binding** for the agent (memory
  ownership, an `RWMutex` that stops `Close` freeing a handle another goroutine
  is inside, C→Go callbacks through `runtime/cgo.Handle`, a C shim rather than C
  in a comment), a link-layer-aware packet parser, and benchmarks of the boundary
  itself (a CGo call costs ~70× a Go call). Capture sits behind the `cgo` build
  tag, so `CGO_ENABLED=0` still yields a complete, statically linked,
  cross-compilable agent. Alongside it, `internal/agent/kernel` reads the same
  class of kernel counters from `/proc/net/snmp` with **no CGo at all** — the
  alternative you are supposed to check first.
- **v0.9.0** — Testing, benchmarking & profiling as a discipline: the test
  pyramid split by build tag (unit/integration/e2e); 17 invariant-based fuzz
  targets; a benchmark suite; `benchcmp` (a from-scratch benchstat with a
  Mann-Whitney U test) and `mutate` (a from-scratch mutation tester on
  `go test -overlay`); a goroutine-leak detector; and pprof moved to its own
  listener. The fuzzers found and fixed real bugs: a **non-injective series
  key** and a matching **alert-fingerprint collision** (two distinct series or
  alerts silently merging), an **unbounded allocation** in WAL replay, an
  **int64 overflow** in the chunk bounds check, a **NaN/Inf** value that gRPC
  accepted but HTTP rejected, and an **unbounded per-agent map** in the rate
  limiter (a memory-exhaustion DoS). CI runs the pyramid per push; a separate
  workflow carries longer fuzzing and mutation testing, dispatchable by hand (its
  nightly cron is committed but left commented, so a fork does not run it
  unattended).
