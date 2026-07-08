# TraceForge

A distributed metrics collection and analysis system in Go (a simplified
Prometheus-like stack). The Go module is `metrics-system`; it ships two
binaries — an **agent** that collects host metrics and a **server** that
ingests, stores and serves them.

## Architecture

```text
              HTTP  POST /api/v1/metrics ──┐
agent(s) ─────┤                            ├──► server
              gRPC  IngestStream (stream) ─┘      │
                                                  ▼
           ingest ─► validate ─► enrich ─► store   (channel pipeline, worker pools)
                                                  │
                                                  ▼
                     storage: memory | bolt | tsdb
                                                  ▲
              HTTP  GET /api/v1/query ────────────┤
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
│   └── server/main.go         # server entry point + config (HTTP + gRPC)
├── internal/
│   ├── agent/                 # collectors, HTTP sender, gRPC streaming sender
│   ├── model/metric.go        # Metric, Batch, MetricType
│   ├── auth/                  # API keys, JWT (HS256/RS256+JWKS), RBAC, tenant principal
│   ├── grpcconv/              # model <-> protobuf conversion
│   ├── proto/metricspb/       # generated protobuf + gRPC code (go generate)
│   └── server/
│       ├── handler.go         # thin HTTP handlers (ingest, query, stats, pprof)
│       ├── middleware.go      # recover, request id, logging, rate limiting
│       ├── server.go          # http.Server + graceful lifecycle
│       ├── grpcserver/        # gRPC service, interceptors, lifecycle
│       ├── pipeline/          # channel pipeline: stages, worker pools, self-stats
│       ├── storage/           # TSDB: series/index/query + memory, bolt, tsdb backends
│       └── ratelimit/         # per-agent token-bucket limiter
└── pkg/
    └── httpx/client.go        # reusable HTTP client with retry + backoff
```

## Build & run

```bash
make tidy
make build          # -> bin/server, bin/agent
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

```bash
make test                                                    # race + coverage
go test -bench=. -benchmem ./internal/server/pipeline/       # throughput benchmark
go tool pprof 'http://localhost:8080/debug/pprof/profile?seconds=10'   # quote for zsh; keep < 15s WriteTimeout
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
