# TraceForge — Usage Scenarios

A catalog of end-to-end flows the system supports, written as runnable recipes.
This file is maintained alongside the staged roadmap: each milestone that adds a
user-visible capability also adds or updates the relevant scenarios here.

- **Covers up to:** v0.6.0 (embedded live dashboard)
- **Last updated:** 2026-07-09

Conventions used below:

- Server listens on HTTP `:8080` and gRPC `:9090` by default.
- Build once with `make build` → `bin/server`, `bin/agent`.
- `$` lines are shell; responses are abbreviated.

---

## 1. Core ingest & query (no auth)

### 1.1 Quickstart — agent ships host metrics, you query them

**Goal:** stand the whole system up and see live host metrics.

```bash
# Terminal 1
./bin/server                       # HTTP :8080 + gRPC :9090, in-memory store
# Terminal 2
./bin/agent                        # collects cpu/mem/disk/uptime every 5s over HTTP
```

```bash
# Raw points for one metric
curl -s 'localhost:8080/api/v1/query?name=cpu_usage_percent' | jq
```

**Expected:** `202 Accepted` on each agent batch; the query returns a growing
list of points, each tagged with an injected `agent_id` label.

### 1.2 Manual ingest of a single batch

**Goal:** push a metric without the agent.

```bash
curl -i -XPOST localhost:8080/api/v1/metrics \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"demo","metrics":[
        {"name":"cpu_usage_percent","type":"gauge","value":12.5,
         "timestamp":"2026-07-09T10:00:00Z"}]}'
```

**Expected:** `202 Accepted`. Ingest is asynchronous — the metric flows through
the pipeline (`ingest → unpack → validate → enrich → store`) before it is
queryable (typically <100 ms).

### 1.3 Filtering & windowed aggregation

**Goal:** compute a per-minute average for one host.

```bash
curl -s 'localhost:8080/api/v1/query?name=memory_used_percent\
&agent_id=web-1&agg=avg&step=1m\
&from=2026-07-09T00:00:00Z&to=2026-07-09T23:59:59Z' | jq
```

- Any query param that is not `name`/`from`/`to`/`agg`/`step`/`limit` is treated
  as a **label filter** (all must match).
- `agg` ∈ `avg|min|max|sum|count|p50|p90|p95|p99`; `step` requires `agg`.
- `limit` caps returned points. Empty/`none`/`raw` `agg` returns raw points.

### 1.4 Backpressure

**Goal:** observe the server shed load when the pipeline is saturated.

```bash
./bin/server -ingest-buffer=1          # tiny buffer to force saturation
# hammer ingest concurrently...
```

**Expected:** over-capacity requests get `503 Service Unavailable` with a
`Retry-After: 1` header (HTTP) or `ResourceExhausted` (gRPC unary). No metric
already accepted is ever lost.

---

## 2. gRPC transport

### 2.1 Agent over gRPC (streaming ingest)

```bash
./bin/server
./bin/agent -transport=grpc -grpc-server=localhost:9090
```

**Expected:** the agent opens **one long-lived bidirectional stream** and sends
each tick's batch on it (reused across ticks, reopened on error); the server
acks every batch. Data is queryable over HTTP or gRPC — both transports feed the
same pipeline and store.

### 2.2 Poke the gRPC API with grpcurl (reflection is on)

```bash
grpcurl -plaintext localhost:9090 list metrics.v1.MetricsService
grpcurl -plaintext -d '{"name":"cpu_usage_percent"}' \
  localhost:9090 metrics.v1.MetricsService/Query
```

RPC styles: `Ingest` (unary), `IngestStream` (bidirectional), `Query`
(server-streaming).

---

## 3. Persistent storage

### 3.1 Choose a backend

```bash
./bin/server -storage=memory                       # default; lost on restart
./bin/server -storage=bolt  -data-dir=./data       # bbolt B+tree file
./bin/server -storage=tsdb  -data-dir=./data       # from-scratch WAL + mmap chunks
```

### 3.2 Survive a restart

**Goal:** confirm data persists across a process restart.

```bash
./bin/server -storage=tsdb -data-dir=./data        # ingest some metrics, then Ctrl+C
./bin/server -storage=tsdb -data-dir=./data        # restart on the same dir
curl -s 'localhost:8080/api/v1/query?name=cpu_usage_percent' | jq length
```

**Expected:** the count is non-zero after restart. For `tsdb`, an in-flight
batch not yet flushed to a chunk is recovered from the WAL on reopen.

---

## 4. Observability & operations

### 4.1 Self-metrics and health

```bash
curl -s localhost:8080/debug/stats | jq   # {pipeline:{ingested,dropped,invalid,stored}, storage:{series,points}}
curl -i localhost:8080/healthz            # 200 "ok"  (always public)
```

### 4.2 Profiling

```bash
go tool pprof 'http://localhost:8080/debug/pprof/profile?seconds=10'   # quote for zsh
```

### 4.3 Per-agent rate limiting

```bash
./bin/server -rate-limit-rps=5 -rate-limit-burst=5
```

**Expected:** a client exceeding its budget (keyed by `X-Agent-ID`, else client
IP) gets `429 Too Many Requests` with `Retry-After`.

### 4.4 Graceful shutdown

**Goal:** stop without losing buffered metrics.

Send `SIGINT`/`SIGTERM` (Ctrl+C). **Expected:** both transports stop accepting,
the pipeline drains every in-flight metric into storage, storage is closed
(WAL flushed, file lock released), then the process exits.

---

## 5. Authentication, RBAC & multi-tenancy

> Auth is **off by default**. Enable with `-auth` plus at least one credential
> source. It guards both HTTP and gRPC identically.

### 5.1 API keys

`api-keys.json`:

```json
{"keys":[
  {"key":"KA-writer","subject":"agent-a","tenant":"tenant-a","roles":["writer"]},
  {"key":"KA-reader","subject":"dash-a","tenant":"tenant-a","roles":["reader"]},
  {"key":"KB-reader","subject":"dash-b","tenant":"tenant-b","roles":["reader"]}
]}
```

```bash
./bin/server -auth -api-keys=./api-keys.json
./bin/agent  -api-key=KA-writer                       # HTTP
./bin/agent  -transport=grpc -api-key=KA-writer       # gRPC (metadata x-api-key)
curl -s -H 'X-API-Key: KA-reader' 'localhost:8080/api/v1/query?name=cpu_usage_percent'
```

### 5.2 RBAC outcomes

| Caller | Action | Result |
|--------|--------|--------|
| no credential | any protected route | `401` / `Unauthenticated` |
| `reader` | `POST /api/v1/metrics` (ingest) | `403` / `PermissionDenied` |
| `writer` | ingest | ✅ |
| `reader` | `GET /api/v1/query` | ✅ |
| `reader`/`writer` | `GET /debug/stats`, `/debug/pprof/*` | `403` (admin only) |
| `admin` | everything | ✅ |
| any | `GET /healthz` | ✅ (public) |

### 5.3 JWT bearer tokens

```bash
# HS256 (shared secret)
./bin/server -auth -jwt-hs256-secret="$SECRET"
curl -H "Authorization: Bearer $JWT" 'localhost:8080/api/v1/query?name=cpu_usage_percent'

# RS256 via a rotating JWKS endpoint
./bin/server -auth -jwks-url=https://issuer.example/.well-known/jwks.json \
             -jwt-issuer=https://issuer.example -jwt-audience=traceforge
```

Token claims used: `sub`, `tenant`, and `roles` (array) or `scope` (space-
delimited). `exp` is mandatory; `iss`/`aud` enforced when configured. The agent
sends a bearer token with `-auth-token=<jwt>`.

### 5.4 Multi-tenant isolation

**Goal:** prove a tenant can only read its own series.

```bash
./bin/server -auth -api-keys=./api-keys.json
./bin/agent -api-key=KA-writer -id=a-agent           # writes as tenant-a
curl -s -H 'X-API-Key: KA-reader' 'localhost:8080/api/v1/query?name=cpu_usage_percent' | jq length  # > 0
curl -s -H 'X-API-Key: KB-reader' 'localhost:8080/api/v1/query?name=cpu_usage_percent'               # []
```

**Expected:** tenant-a reader sees the data (each point carries a server-assigned
`tenant=tenant-a` label); tenant-b reader sees `[]`. The `tenant` label is
server-controlled — a client cannot set or spoof it.

---

## 6. Live dashboard

### 6.1 Watch metrics in the browser

**Goal:** see metrics update live.

```bash
./bin/server            # dashboard at http://localhost:8080/
./bin/agent             # feed it
```

Open `http://localhost:8080/`. **Expected:** the metric feed fills as batches are
stored, and the counters + chart update every ~2s. The page reconnects
automatically if the server restarts.

### 6.2 Dashboard with auth on

**Goal:** a tenant sees only its own live metrics.

```bash
./bin/server -auth -api-keys=./api-keys.json
```

Open the dashboard, paste an API key (or JWT) into the credential field, and
Connect. **Expected:** the WebSocket stream is scoped to that credential's
tenant; the counters/chart appear only for an admin credential. A missing/invalid
credential is rejected at the handshake (`401`).

### 6.3 Disable the UI

```bash
./bin/server -ui=false     # no dashboard, no /ws; API + gRPC unchanged
```

## 7. Failure & edge flows

| Flow | Trigger | Result |
|------|---------|--------|
| Malformed JSON on ingest | bad body / unknown field / trailing data | `400 {"error":"invalid json"}` |
| Oversized body | ingest body > 1 MiB | `400` (capped reader) |
| Invalid metric | empty name, bad type, NaN/Inf value | dropped in the pipeline, counted in `stats.invalid` |
| Query without `name` | `GET /api/v1/query` | `400 name is required` |
| Expired / bad-signature JWT | `Authorization: Bearer <bad>` | `401` |
| Wrong storage type | `-storage=foo` | startup error, non-zero exit |
| gRPC without credentials (auth on) | any RPC | `Unauthenticated` |

---

## Maintenance note

When a new stage lands, add its user-visible flows here and bump the "Covers up
to" line. History of what each milestone added lives in `CHANGELOG.md` and the
wiki `Roadmap`.
