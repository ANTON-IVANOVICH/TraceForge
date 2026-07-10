# TraceForgeHighLatency

## What fired

```promql
histogram_quantile(
  0.99,
  sum by (le, route) (rate(traceforge_http_request_duration_seconds_bucket[5m]))
) > 1
```

The estimated 99th-percentile request latency on some route is above one second,
sustained for ten minutes. One in a hundred requests to that route is taking
longer than a second to complete.

## Impact

Slow reads and slow ingest for the affected route. If it is `/api/v1/metrics`,
agents wait on each POST; with a tight `-http-timeout` they start timing out and
retrying, which adds load and can tip the pipeline into
[dropping](TraceForgePipelineDropping.md). If it is `/api/v1/query`, the
dashboard and any alert evaluation reading from this server get slow or time out.
p99 above a second usually means the server is contended (CPU, GC, a serialising
lock) or a downstream — storage — has slowed, and it is felt by real clients
before it shows anywhere else.

Read the `route` label carefully. It is the mux's registered **pattern**, not
the request path, so `/api/v1/query` is one series no matter the query string.
`route="other"` is the catch-all for unmatched paths (404s, 405s, redirects);
high p99 there is meaningless — those "requests" are attacker-chosen paths and
error responses, not a real endpoint — so **ignore `route="other"`** and look at
the real routes. Also note `/ws` is excluded from the duration histogram by
design: a hijacked WebSocket lives as long as the browser tab, and feeding its
lifetime into the histogram would drag the whole server's p99 to "one hour".

## Diagnose

1. **Which route is slow?** The alert already groups by route; read it back to
   find the culprit, ignoring `other`:

   ```promql
   histogram_quantile(0.99,
     sum by (le, route) (rate(traceforge_http_request_duration_seconds_bucket[5m])))
   ```

2. **Is it the whole distribution or just the tail?** Compare p50 and p99 on the
   same route — the discriminating query for "everything is slow" versus "the
   tail is slow":

   ```promql
   histogram_quantile(0.50, sum by (le) (rate(traceforge_http_request_duration_seconds_bucket{route="/api/v1/query"}[5m])))
   histogram_quantile(0.99, sum by (le) (rate(traceforge_http_request_duration_seconds_bucket{route="/api/v1/query"}[5m])))
   ```

   - p50 low, p99 high → a tail problem: GC pauses, lock contention, occasional
     large queries, or one slow replica.
   - p50 and p99 both high → systemic: the route is slow for everyone, usually
     CPU starvation or a slow storage backend.

3. **Is it one replica?** Per-instance p99 tells you whether to fix a pod or the
   fleet:

   ```promql
   histogram_quantile(0.99, sum by (le, instance) (rate(traceforge_http_request_duration_seconds_bucket{route="/api/v1/query"}[5m])))
   ```

4. **Is storage the wall, or the runtime?** Cross-check the runtime signals and
   the pipeline. Rising GC work and memory pressure show up as latency:

   ```promql
   rate(traceforge_go_gc_cycles_total[5m])
   traceforge_go_memory_total_bytes / (traceforge_go_memory_limit_bytes < 9e18)
   ```

   If the server is near GOMEMLIMIT the GC is stealing CPU from handlers — see
   [TraceForgeNearMemoryLimit](TraceForgeNearMemoryLimit.md). If ingest latency
   is high and `stored` is lagging `ingested`, the store stage / disk is the
   bottleneck, same root cause as
   [TraceForgePipelineDropping](TraceForgePipelineDropping.md).

5. **Is the pod CPU-throttled?** Check the container's CPU throttling from the
   node/cgroup metrics. A throttled pod adds latency to everything and no code
   change fixes it — it needs more CPU quota.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| CPU starvation / throttling | Container CPU at its limit, cgroup throttling non-zero; p50 and p99 both up | Raise the CPU limit; scale out replicas |
| GC pressure near GOMEMLIMIT | `traceforge_go_gc_cycles_total` rate climbing, memory ratio high | Reduce series or raise the memory limit — see [TraceForgeNearMemoryLimit](TraceForgeNearMemoryLimit.md) |
| Slow storage backend | Ingest p99 high, `stored` rate < `ingested`, disk write latency high | Faster disk; fewer series/points; see [TraceForgePipelineDropping](TraceForgePipelineDropping.md) |
| Expensive queries | p99 high only on `/api/v1/query`, p50 fine; large time ranges / no aggregation in logs | Narrow query windows; add `--agg`/`--step`; rate-limit heavy readers |
| One slow replica | Per-instance p99 singles out one pod (bad node, local disk) | Evict the pod; cordon the node |
| pprof left on under load | `-pprof-addr` set and being scraped continuously | pprof is loopback-only and off by default; disable continuous profiling in prod |

## Mitigate

**Stop the bleeding (next five minutes):**

- If one replica is the outlier, evict it: `kubectl delete pod POD` (confirm the
  Service keeps healthy endpoints first). The fleet p99 recovers immediately.
- If the whole route is slow and it is CPU, give the pods more CPU or scale out
  — latency under contention falls fastest by adding capacity, not by tuning.
- If expensive `/api/v1/query` calls are the cause, throttle the heavy reader at
  the source and narrow its window; one unbounded query can dominate a route's
  tail.

**Fix the cause:** raise CPU/memory limits, move to faster storage, or bound the
expensive queries — per the table. Confirm recovery: the route's p99 drops back
below one second and holds.

## Escalate

Escalate to the service owner when p99 stays high after scaling and it is not a
single replica, when storage latency is the proven cause and the disk cannot be
upgraded quickly, or when a specific query pattern is overloading reads and needs
a product fix. Gather first: the per-route p99, the p50-vs-p99 comparison on the
hot route, the per-instance p99, the GC-cycle and memory-ratio queries, and the
container CPU-throttling numbers.

## Why this alert exists

p99 is the number a user actually feels, and it is computed with
`histogram_quantile` over the exported **buckets** — never a summary's
precomputed quantile. That choice is deliberate and load-bearing: histogram
bucket counts are additive, so you can `sum by (le)` across every replica and get
a fleet-wide p99, whereas a per-replica precomputed quantile cannot be averaged
into anything meaningful. That is why this server exports a histogram and no
summary at all. The alert groups by `route` so a slow endpoint is named rather
than buried in an aggregate, and the ten-minute `for` keeps a single slow GC
pause or a burst of big queries from paging.
