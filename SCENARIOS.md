# TraceForge — Usage Scenarios

A catalog of end-to-end flows the system supports, written as runnable recipes.
This file is maintained alongside the staged roadmap: each milestone that adds a
user-visible capability also adds or updates the relevant scenarios here.

- **Covers up to:** v0.10.0 (CGo: libpcap binding, and the alternatives to it)
- **Last updated:** 2026-07-09

Conventions used below:

- Server listens on HTTP `:8080` and gRPC `:9090` by default.
- Build once with `make build` → `bin/server`, `bin/agent`, `bin/metricsctl`.
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

---

## 7. Alerting

> Off by default; enable with `-alerting`. Without `-alert-config` a single
> `log` receiver is used, so the whole stack is demonstrable with no external
> service.

### 7.1 Fire an alert end to end

**Goal:** watch a rule go from quiet to firing to resolved.

```bash
./bin/server -alerting \
  -alert-rules=./examples/alerting/rules.json \
  -alert-config=./examples/alerting/receivers.json
./bin/agent
```

**Expected:** once `cpu_usage_percent` has stayed above 90 for the rule's `for`
window, the server logs an `alert notification` and posts a signed webhook. When
CPU drops — or the agent stops and the series goes stale past `-alert-lookback` —
a second notification arrives with `"status":"resolved"`.

### 7.2 The rule DSL

```text
cpu_usage_percent > 90 for 5m                       # comparison filters: the breaching samples
avg_over_time(memory_used_percent[5m]) > 95         # range function over a window
max by (agent_id) (disk_used_percent) > 85          # aggregate, then compare
rate(http_requests_total[1m]) > 1000                # per-second rate, counter resets handled
up{env=~"prod.*"} == 0 unless maintenance_mode == 1 # matchers + set operations
```

- `for` may live in the rule (`"for": "5m"`) or in the expression text.
- Regexes are **fully anchored**: `env=~"prod"` does not match `production`.
- Every alert gets `alertname` and `severity` labels unless the rule sets them.
- Annotations are templates: `"summary": "CPU is {{ .Value }}% on {{ .Labels.agent_id }}"`.

### 7.3 Manage rules at runtime (hot reload)

```bash
curl -s localhost:8080/api/v1/rules | jq
curl -s -XPOST localhost:8080/api/v1/rules -H 'Content-Type: application/json' -d '{
  "name":"MemoryCritical",
  "expression":"avg_over_time(memory_used_percent[5m]) > 95",
  "for":"2m", "interval":"30s", "severity":"critical", "receivers":["log"]
}' | jq
curl -s -XDELETE localhost:8080/api/v1/rules/<id> -i
```

**Expected:** `201 Created`, and the rule starts evaluating immediately — no
restart. A bad expression is rejected at creation with `400` and a byte position,
e.g. `parse error at position 20: expected an expression but found end of input`.

Deleting (or disabling) a rule that is currently firing **announces the
resolution** first, so receivers are told the incident is over instead of being
reminded about it forever. `tenant` is a reserved rule label and is rejected.

### 7.4 Backtest before you commit (`preview`)

**Goal:** see what a rule *would* have fired on, before saving it.

```bash
curl -s -XPOST localhost:8080/api/v1/rules/preview -H 'Content-Type: application/json' -d '{
  "expression":"cpu_usage_percent > 80",
  "from":"2026-07-09T10:00:00Z","to":"2026-07-09T11:00:00Z","step":"1m"
}' | jq
```

**Expected:** `{"results":[{"at":…,"samples":[…]}],"count":N}` — one entry per
step at which the expression matched. The window is capped at 500 steps.

### 7.5 Inspect active alerts

```bash
curl -s localhost:8080/api/v1/alerts | jq '.[] | {state, labels, value}'
```

States: `pending` (the condition holds but `for` has not elapsed) and `firing`.
The dashboard shows the same set in its **Alerts** panel, updated live.

### 7.6 Silence an alert during maintenance

```bash
curl -s -XPOST localhost:8080/api/v1/silences -H 'Content-Type: application/json' -d '{
  "matchers":[{"name":"agent_id","op":"=","value":"web-1"}],
  "duration":"2h","created_by":"anton","comment":"planned maintenance"
}' | jq
curl -s localhost:8080/api/v1/silences | jq
curl -s -XDELETE localhost:8080/api/v1/silences/<id> -i
```

Matcher ops: `=`, `!=`, `=~`, `!~`. **Expected:** matching alerts stop producing
notifications but still appear in `GET /api/v1/alerts` and on the dashboard —
silencing hides the page, not the problem. A silence created *after* an alert was
already grouped still suppresses its repeat reminders. A silence with **no
matchers** is rejected (`400`): it would mute every alert in the system.

### 7.7 Suppress follow-up alerts (inhibition)

In the alerting config:

```json
{"inhibit_rules":[{
  "source_matchers":[{"name":"alertname","op":"=","value":"HostDown"}],
  "target_matchers":[{"name":"severity","op":"=~","value":"warning|critical"}],
  "equal":["agent_id"]
}]}
```

**Expected:** while `HostDown` fires for `agent_id=web-1`, other alerts on
`web-1` are suppressed. A dead host's CPU being strange is not news.

### 7.8 Receivers, grouping and delivery

```json
{
  "group_by": ["alertname", "tenant"],
  "group_wait": "30s", "group_interval": "5m", "repeat_interval": "4h",
  "default_receivers": ["log"],
  "receivers": [
    {"name":"log","type":"log"},
    {"name":"hook","type":"webhook","url":"https://ops.example/alerts","secret":"…"},
    {"name":"slack","type":"slack","webhook_url":"https://hooks.slack.com/…"},
    {"name":"oncall","type":"email","host":"smtp.example.com","port":587,
     "smtp_username":"u","password":"p","from":"a@b.c","to":["oncall@b.c"]}
  ]
}
```

- `group_wait` — how long to let an incident coalesce before the first
  notification, so fifty failing hosts become **one** message.
- `group_interval` — how soon an *updated* group may notify again.
- `repeat_interval` — how often an *unchanged* group is re-sent as a reminder.
- Each receiver has its own **circuit breaker**, so a dead SMTP server cannot
  stall Slack. Failures retry with exponential backoff **and jitter**; a `4xx`
  other than `408`/`429` is permanent and dropped without retry.

### 7.9 Verify a webhook signature

Every webhook carries `X-TraceForge-Timestamp` and
`X-TraceForge-Signature: sha256=<hex>`, where the HMAC covers
`"<timestamp>.<body>"` — the timestamp is signed too, so a captured request
cannot be replayed later.

```python
mac = hmac.new(secret.encode(), (ts + "." + body).encode(), hashlib.sha256)
assert hmac.compare_digest("sha256=" + mac.hexdigest(), signature)
```

### 7.10 Alerting with auth on

```bash
./bin/server -auth -api-keys=./api-keys.json -alerting
curl -H 'X-API-Key: KA-reader' localhost:8080/api/v1/alerts                # ✅ query action
curl -H 'X-API-Key: KA-reader' -XPOST localhost:8080/api/v1/rules -d '…'   # 403 (admin only)
curl -H 'X-API-Key: KA-admin'  -XPOST localhost:8080/api/v1/rules -d '…'   # ✅ 201
```

**Expected:** a rule's tenant comes from the credential, never from the body. A
rule evaluates only against its own tenant's series, and tenant-b gets `404` for
tenant-a's rule ID — `404` rather than `403`, so IDs cannot be probed.

### 7.11 Disable alerting

```bash
./bin/server            # no rule evaluation, no /api/v1/rules|alerts|silences routes
```

---

## 8. The CLI — `metricsctl`

### 8.1 First run

```bash
make build                                # -> bin/metricsctl
./bin/metricsctl config set-context local --server http://localhost:8080 --use
./bin/metricsctl config get-contexts
./bin/metricsctl stats
```

**Expected:** the config is created at `~/.metricsctl/config.yaml` with mode
`0600`. If it is ever loosened, every invocation warns — it holds credentials.

### 8.2 Several deployments, one binary

```yaml
# ~/.metricsctl/config.yaml
current-context: local
contexts:
  local:
    server: http://localhost:8080
  prod:
    server: https://metrics.example.com
    ca-file: ~/.metricsctl/prod-ca.pem
    auth:
      token-file: ~/.metricsctl/prod.token     # or: token: ${METRICSCTL_TOKEN}
```

```bash
metricsctl --context prod alerts list      # one-off, without switching
metricsctl config use-context prod         # or switch for good
metricsctl config view                     # credentials print as REDACTED
```

`${VAR}` in the config is expanded from the environment, so a checked-in config
carries placeholders rather than secrets. `~` and config-relative paths resolve.

### 8.3 Query metrics

```bash
metricsctl query cpu_usage_percent
metricsctl query cpu_usage_percent -l agent_id=web-1 --from -1h --agg avg --step 1m
metricsctl query cpu_usage_percent -o json | jq '.[].value'
```

`--from`/`--to` accept RFC3339 or a relative offset (`-1h`, `-30m`, `now`).

### 8.4 Output formats compose with the shell

```bash
metricsctl rules list                      # aligned table for humans
metricsctl rules list -o json | jq '.[] | select(.enabled==false) | .id'
metricsctl rules get cpu-high -o yaml > cpu-high.yaml
metricsctl rules list -o name | xargs -n1 metricsctl rules get -o yaml
```

**Expected:** `-o json`/`-o yaml` encode the raw API object — never the table
projection — and a list stays a list even with one element. `-o name` prints one
identifier per line. Colour never reaches a pipe or a file.

### 8.5 Declarative rules (`apply`)

`rules.yaml` (see [examples/alerting/rules.yaml](examples/alerting/rules.yaml)):

```yaml
apiVersion: v1
kind: Rule
metadata:
  name: cpu-high            # the rule's stable id — this is what makes apply idempotent
spec:
  expression: cpu_usage_percent > 90
  for: 1m
  interval: 15s
  severity: warning
  receivers: [log]
  annotations:
    summary: "CPU is {{ .Value }}% on {{ .Labels.agent_id }}"
---
apiVersion: v1
kind: Rule
metadata:
  name: memory-critical
spec:
  expression: avg_over_time(memory_used_percent[5m]) > 95 for 2m
  severity: critical
```

```bash
metricsctl rules apply -f rules.yaml --dry-run   # validate, write nothing
metricsctl rules apply -f rules.yaml
metricsctl rules apply -f rules.yaml             # again: everything "unchanged"
cat rules.yaml | metricsctl rules apply -f -     # from stdin, for a pipeline
```

**Expected:** a diff-style report — `created` / `updated` / `unchanged` — so it
is obvious what actually happened. Re-applying an unchanged file writes nothing,
which is what makes `apply` safe to run from CI on every push.

The manifest is the **desired state in full**: delete a field and the next apply
reconciles it back to the server's default (`interval` 15s, `severity` warning,
`enabled` true). Expressions are compiled locally before any request, so
`--dry-run` rejects a bad rule even when it would only have been an update.

### 8.6 Backtest before you commit

```bash
metricsctl rules preview 'cpu_usage_percent > 80' --from -1h --step 1m
```

### 8.7 Watch what is on fire

```bash
metricsctl alerts list
metricsctl alerts list --state firing -o json
metricsctl alerts list --watch --interval 2s     # redraws until Ctrl+C
```

**Expected:** firing alerts sort above pending ones. `--watch` needs the table
format and a terminal; Ctrl+C exits `0`, not as an error.

### 8.8 Silences and agents

```bash
metricsctl silences create -m agent_id=web-1 --duration 2h --comment "maintenance"
metricsctl silences create -m 'env=~prod.*' -m severity=warning --duration 30m
metricsctl silences list
metricsctl silences delete <id> --yes

metricsctl agents list
metricsctl agents list -o name | xargs -n1 -I{} metricsctl query cpu_usage_percent -l agent_id={}
```

The server keeps no agent registry, so `agents list` derives one from a heartbeat
metric (`uptime_seconds` by default; change it with `--heartbeat`). An agent
silent for more than two minutes shows as `stale`.

### 8.9 Exit codes are the contract

```bash
metricsctl rules get nope || echo "exit $?"     # 4 — not found
metricsctl --token bad alerts list              # 3 — auth
metricsctl rules list -o xml                    # 2 — usage
metricsctl rules deletee cpu-high               # 2 — a typo is a usage error, not a silent 0
metricsctl stats                                # 1 — server unreachable
```

`0` success, `1` generic failure, `2` usage error, `3` authentication or
authorization, `4` not found. That is what makes
`metricsctl rules get foo || handle_missing` reliable in a script.

### 8.10 Scripts, prompts and colour

```bash
metricsctl rules delete cpu-high            # prompts on a terminal
metricsctl rules delete cpu-high --yes      # required in a script
NO_COLOR=1 metricsctl alerts list           # or --no-color
```

**Expected:** a destructive command run without a terminal and without `--yes`
exits `2` with an explanation rather than hanging on a prompt nobody can answer
or silently destroying things. Colour requires a real terminal — `/dev/null` is
a character device but not a terminal, and gets no escape codes.

### 8.11 Shell completion

```bash
source <(metricsctl completion bash)
metricsctl completion zsh > "${fpath[1]}/_metricsctl"
metricsctl completion fish > ~/.config/fish/completions/metricsctl.fish
```

Completion is **dynamic**: `metricsctl rules get <TAB>` asks the server which
rules exist; `metricsctl --context <TAB>` lists the configured contexts.

---

## 9. Failure & edge flows

| Flow | Trigger | Result |
|------|---------|--------|
| Malformed JSON on ingest | bad body / unknown field / trailing data | `400 {"error":"invalid json"}` |
| Oversized body | ingest body > 1 MiB | `400` (capped reader) |
| Invalid metric | empty name, bad type, NaN/Inf value | dropped in the pipeline, counted in `stats.invalid` |
| Query without `name` | `GET /api/v1/query` | `400 name is required` |
| Expired / bad-signature JWT | `Authorization: Bearer <bad>` | `401` |
| Wrong storage type | `-storage=foo` | startup error, non-zero exit |
| gRPC without credentials (auth on) | any RPC | `Unauthenticated` |
| Bad rule expression | `POST /api/v1/rules` | `400 parse error at position N: …` |
| `rate(cpu)` without a range | rule creation | `400 … requires a range vector selector like metric[5m]` |
| Preview window too large | `from`/`to`/`step` > 500 steps | `400 preview window too large` |
| Silence with no matchers | `POST /api/v1/silences` | `400` (it would mute everything) |
| Rule declaring a `tenant` label | `POST /api/v1/rules` | `400` (reserved, server-controlled) |
| Receiver down | webhook/SMTP failing | retried with backoff + jitter, then the circuit opens |
| Bad rules/receivers file | `-alert-rules`, `-alert-config` | startup error, non-zero exit |
| Unknown CLI context | `metricsctl --context nope ...` | exit `4` |
| Bad CLI output format | `metricsctl rules list -o xml` | exit `2` |
| Destructive CLI command in a script | `metricsctl rules delete x` (no TTY, no `--yes`) | exit `2`, nothing deleted |
| Bad rule manifest | `metricsctl rules apply -f bad.yaml` | exit `2`, nothing written |
| Loose config permissions | `chmod 644 ~/.metricsctl/config.yaml` | warning on every invocation |
| Series with delimiters in labels | `{a: "b,c=d"}` vs `{a: "b", c: "d"}` | stored as two distinct series (escaped, injective key) |
| Non-finite value over gRPC | a NaN/Inf metric via the gRPC transport | rejected by `Validate`, same as the HTTP path |
| Corrupt / torn WAL on restart | crash mid-write, or a flipped byte | replay stops cleanly at the bad record; the process starts |
| Hostile WAL record length | a header claiming a 4 GiB payload | treated as corruption, not allocated |
| Rate-limiter key flood | a stream of distinct agent ids / IPs | bucket map capped and swept, not grown until OOM |
| Storage write failure | `WriteBatch` errors (full disk) | metrics counted in `stats.failed`, surfaced on the dashboard |
| pprof on the API port | `GET /debug/pprof/` without `-pprof-addr` | `404` — profiling is a separate, opt-in listener |
| Capture without privileges | `agent -network -network-device=en0` as a normal user | warning logged, agent keeps reporting CPU/memory/disk |
| Capture in a no-CGo binary | `CGO_ENABLED=0` agent with `-network` | warning logged, agent runs on |
| `-network` with no target | neither `-network-device` nor `-network-file` | warning logged, agent runs on |
| Invalid BPF filter | `-network-filter='not a filter'` | capture fails to open; collector disabled, agent runs on |
| Cross-compiling with CGo | `make cross-cgo` | build error — the host toolchain cannot target another platform |

---

## 10. Network metrics via CGo (libpcap)

The agent's one crossing into C. It is off by default and every failure is
non-fatal, because an agent that will not start because it could not open a raw
socket reports nothing at all.

```bash
# A savefile: no privileges needed, and how the package is tested.
./bin/agent -network -network-file=capture.pcap -network-filter=

# A live interface: root on macOS (/dev/bpf*), CAP_NET_RAW on Linux.
sudo ./bin/agent -network -network-device=en0 -network-filter='ip or ip6'

# The filter is compiled and applied in the kernel, so non-matching packets
# never cross into user space at all.
sudo ./bin/agent -network -network-device=eth0 -network-filter='tcp port 443'
```

```bash
curl -s 'http://localhost:8080/api/v1/query?name=net_protocol_packets_total' | jq '.[] | {p:.labels.protocol, v:.value}'
# {"p":"tcp","v":3} {"p":"udp","v":2} {"p":"icmp","v":1}
```

`net_bytes_total` counts the wire length, not the truncated copy;
`net_kernel_dropped_total` reports what the kernel discarded before this process
saw it — without it, the other counters go quiet exactly when the network is
busiest.

**The CGo tax, and how to avoid paying it:**

```bash
make build           # with capture; needs libpcap on the host
make build-nocgo     # CGO_ENABLED=0 — complete agent, capture reports unavailable
make cross-nocgo     # cross-compiles to 4 platforms from nothing
make cross-cgo       # fails on purpose: the error is the lesson
make test-nocgo      # the suite with the C compiler taken away
go test -bench . ./internal/agent/network/   # what a border crossing costs
```

A CGo call is ~20ns against ~0.3ns for a Go call. That single number decides the
shape of every binding: cross rarely, do much on the far side.

**And the alternative you should check first** — the same class of kernel
counters, with no C at all:

```bash
# Linux only; reads /proc/net/snmp and /proc/net/netstat.
curl -s 'http://localhost:8080/api/v1/query?name=tcp_retransmit_segments_total'
```

---

## 11. Developer workflows — testing, benchmarking, profiling

The whole point of stage 9: the tools an engineer reaches for when a green build
is not enough. All are `make` targets; none pull in an external dependency.

```bash
# The test pyramid, by build tag.
make test              # unit: -race, milliseconds each — the base
make test-integration  # //go:build integration: real bbolt/tsdb, httptest, bufconn
make test-e2e          # //go:build e2e: builds the binaries, runs them as processes
make cover             # atomic-mode coverage; `make cover-html` to browse it

# Fuzzing. A plain `go test` already replays every committed crasher; these hunt
# for new inputs. The targets assert invariants (round-trip, injectivity), so a
# failure is a real bug, not a stray panic.
make fuzz                    # 15s per target — CI-on-PR depth
make fuzz FUZZTIME=10m       # the nightly depth

# Benchmarking with a significance test. One run is an anecdote; ten are a
# sample. benchcmp reports `~` when the difference is within the noise.
git switch main    && make bench-save NAME=base
git switch mybranch && make bench-save NAME=new
make bench-compare OLD=base NEW=new
#   SeriesKey/labels=3   156.9 ± 3%   128.0 ± 1%   -18.48% (p=0.000 n=10+10)
#   SeriesKey/labels=1    79.6 ± 3%    81.4 ± 3%   ~ (p=0.054 n=10+10)

# Mutation testing: does the suite check the lines it covers?
make mutate PKG=./internal/alerting/rules
#   SURVIVED  rules/manager.go:78: conditional: > -> >=
#   mutation score: 84.2% (...)

# Profiling. pprof is a separate listener, off by default, bound to loopback.
./bin/server -pprof-addr=127.0.0.1:6060 &
make profile-cpu   PROF_PKG=./internal/server/storage   # flame graph from a benchmark
make profile-trace PROF_PKG=./internal/server/pipeline  # go tool trace: why it won't scale
go tool pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=10'
```

The bugs this stage's fuzzers and benchmarks turned up — a non-injective series
key, an alert-fingerprint collision, an unbounded WAL allocation, a chunk
overflow, a gRPC-only NaN, an unbounded rate-limiter map — are in the failure
table above and detailed in `CHANGELOG.md`.

---

## Maintenance note

When a new stage lands, add its user-visible flows here and bump the "Covers up
to" line. History of what each milestone added lives in `CHANGELOG.md` and the
wiki `Roadmap`.
