# TraceForge runbooks

One file per alert. The `runbook_url` in every rule in
[`deploy/prometheus/alerts.yml`](../../deploy/prometheus/alerts.yml) names the
exact file here, and a test in `test/deploy` fails the build if any of them is
missing. A link that 404s at 03:00 is worse than no link.

## The house rule

An alert without a runbook is a page without an answer. Every rule that can wake
someone links a file below, and every file is written for a specific reader: the
engineer who did **not** write this code and was asleep ninety seconds ago. That
means no "investigate the issue" filler — real commands, the exact PromQL, the
one query that tells two causes apart, and the five minutes that stop the
bleeding before anyone understands the cause.

## The alerts

| Alert | Severity | What it means |
|-------|----------|---------------|
| [TraceForgeServerDown](TraceForgeServerDown.md) | critical | Prometheus cannot scrape the telemetry port for 2m. |
| [TraceForgeStorageWritesFailing](TraceForgeStorageWritesFailing.md) | critical | Metrics accepted with 202 are failing to reach storage — silent data loss. |
| [TraceForgePipelineDropping](TraceForgePipelineDropping.md) | warning | The ingest door is shedding load; callers get 429. |
| [TraceForgeHighErrorRate](TraceForgeHighErrorRate.md) | warning | Over 5% of HTTP requests return 5xx. |
| [TraceForgeHighLatency](TraceForgeHighLatency.md) | warning | p99 request latency on a route is above 1s. |
| [TraceForgeNearMemoryLimit](TraceForgeNearMemoryLimit.md) | warning | The runtime is within 10% of GOMEMLIMIT. |
| [TraceForgeRateLimiterBucketsHigh](TraceForgeRateLimiterBucketsHigh.md) | warning | An implausible number of live rate-limit buckets. |
| [TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md) | warning | An agent's collectors are erroring; it sends nothing. |
| [TraceForgeAgentStale](TraceForgeAgentStale.md) | warning | An agent is up and scrapeable but has produced no metric in 2m. |
| [TraceForgeAgentCannotReachServer](TraceForgeAgentCannotReachServer.md) | warning | An agent collects fine but its transport to the server is failing. |

## Orientation for any TraceForge page

Two facts save time on nearly every one of these.

**The server has two listeners.** The API and embedded dashboard live on
`-addr` (default `:8080`); gRPC ingest on `-grpc-addr` (`:9090`). The three
probes and `/metrics` live on a **separate** listener, `-telemetry-addr`
(`:9091`). The agent's probes and `/metrics` are on its own `-telemetry-addr`
(`:9101`). One can be healthy while the other is not.

**The probes mean distinct things.**

- `/healthz` — liveness. 200 for as long as the process runs; depends on nothing. If this fails, the process is gone or wedged.
- `/readyz` — readiness. 200 only when started, the gate is open, and **every** registered check passes. The server registers a `storage` check backed by `store.Ping`; the agent registers a `collectors` check that only passes after a tick has produced a metric.
- `/startupz` — 503 until initialisation finishes, 200 for ever after.
- `/metrics` — Prometheus text exposition.

The distroless images carry no shell and no curl. The container `HEALTHCHECK`
is the binary's own `-health-check` flag, which probes `/readyz` on
`-telemetry-addr` and exits 0 or 1.
