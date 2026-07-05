# TraceForge

A distributed metrics collection and analysis system in Go (a simplified
Prometheus-like stack). The Go module is `metrics-system`; it ships two
binaries — an **agent** that collects host metrics and a **server** that
ingests, stores and serves them.

## Architecture

```text
agent(s) ──POST /api/v1/metrics──► server
                                     │
            ingest ─► validate ─► enrich ─► store   (channel pipeline, worker pools)
                                     │
                                     ▼
                        in-memory TSDB (series + name index)
                                     ▲
GET /api/v1/query ───────────────────┘
```

- **Agent** — every N seconds collects CPU, RAM, disk and uptime, packs them
  into a `Batch` and ships it over HTTP with retry/backoff.
- **Server** — accepts batches into a channel pipeline
  (`ingest → validate → enrich → store`) with configurable worker pools and
  backpressure (a full ingest buffer returns HTTP 503). Metrics land in an
  in-memory time-series store (one series per name+labels, indexed by name)
  and are read back through a query API with label filtering and aggregations.

## Layout

```text
.
├── cmd/
│   ├── agent/main.go          # agent entry point
│   └── server/main.go         # server entry point + config
├── internal/
│   ├── agent/                 # collectors (cpu/memory/disk/uptime), sender, orchestration
│   ├── model/metric.go        # Metric, Batch, MetricType
│   └── server/
│       ├── handler.go         # thin HTTP handlers (ingest, query, stats, pprof)
│       ├── middleware.go      # recover, request id, logging, rate limiting
│       ├── server.go          # http.Server + graceful lifecycle
│       ├── pipeline/          # channel pipeline: stages, worker pools, self-stats
│       ├── storage/           # in-memory TSDB: series, name index, query + aggregations
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

- `-addr` / `SERVER_ADDR` — listen address (default `:8080`)
- `-log-level` / `SERVER_LOG_LEVEL` — `debug|info|warn|error` (default `info`)
- `-storage` / `STORAGE` — backend: `memory` | `bolt` | `tsdb` (default `memory`)
- `-data-dir` / `DATA_DIR` — data directory for the `bolt`/`tsdb` backends (default `./data`)
- `-ingest-buffer` / `INGEST_BUFFER` — ingest channel buffer / backpressure (default `1000`)
- `-validate-workers` / `VALIDATE_WORKERS` — validate stage workers (default `NumCPU`)
- `-enrich-workers` / `ENRICH_WORKERS` — enrich stage workers (default `NumCPU`)
- `-store-workers` / `STORE_WORKERS` — store stage workers (default `1`)
- `-rate-limit-rps` / `RATE_LIMIT_RPS` — per-agent requests/second (default `100`)
- `-rate-limit-burst` / `RATE_LIMIT_BURST` — per-agent burst (default `200`)

### Agent

- `-server` / `AGENT_SERVER` — default `http://localhost:8080/api/v1/metrics`
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
