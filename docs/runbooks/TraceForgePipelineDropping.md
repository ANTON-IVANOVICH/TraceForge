# TraceForgePipelineDropping

## What fired

```promql
rate(traceforge_pipeline_dropped_total[5m]) > 1
```

The server is refusing metrics at the ingest door because the pipeline's ingest
buffer is full. More than one metric per second, sustained over ten minutes, was
turned away before it entered the pipeline.

## Impact

Load shedding, honestly signalled. A `dropped` metric was **refused at the
door**: `Ingest` returned false and the caller received a **429**. The agent
knows its batch was rejected — nobody was lied to, and an agent that retries will
add to `dropped` on each attempt but may still get every metric stored in the
end. So "dropped is rising" means **"the server is shedding load"**, not "data is
gone".

This is the crucial distinction from
[TraceForgeStorageWritesFailing](TraceForgeStorageWritesFailing.md): that alert
counts metrics accepted with 202 and then lost silently. This one counts metrics
we openly declined. Confirm which you have before doing anything else — the fixes
diverge.

The accounting: `dropped` is *not* in the drain identity
`ingested == stored + invalid + failed`, because a dropped metric was never
ingested. Its own identity is per-attempt: `offered == ingested + dropped`.

## Diagnose

1. **Confirm it is dropped, not failed.** This is the discriminating query:

   ```promql
   sum(rate(traceforge_pipeline_dropped_total[5m]))   # refused at the door, caller got 429
   sum(rate(traceforge_pipeline_failed_total[5m]))     # accepted then lost — a different, worse alert
   ```

   If `failed` is also non-zero, stop and work
   [TraceForgeStorageWritesFailing](TraceForgeStorageWritesFailing.md) first —
   silent loss outranks honest backpressure.

2. **Find the bottleneck stage.** Drops happen when the ingest buffer stays full,
   and the buffer stays full when a downstream stage cannot keep up. The store
   stage is the usual culprit because `-store-workers` defaults to **1** — the
   bolt and tsdb backends serialise writers, so one worker is deliberate, not an
   oversight. Compare the flow:

   ```promql
   sum(rate(traceforge_pipeline_ingested_total[5m]))   # entering
   sum(rate(traceforge_pipeline_stored_total[5m]))      # leaving into storage
   ```

   Ingested materially outrunning stored means the store stage is the wall.

3. **Is the disk the real limit?** A slow disk caps store throughput no matter
   how many workers you allow. Check store-stage latency in the logs and the
   volume's write latency/IOPS from the node or CSI metrics. If the disk is
   saturated, adding workers will not help — they will contend on the same
   serialised writer.

4. **Is it a burst or a new steady state?** Look at the ingest rate over the last
   hour. A step change lines up with a new agent fleet, a shortened
   `-interval` on agents, or a traffic spike; a slow climb is organic growth
   the current sizing has outgrown.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Store stage is the bottleneck (default `-store-workers 1`) | `ingested` rate >> `stored` rate; store latency high in logs | Raise `-store-workers` **only if the backend and disk can take concurrent writers**; otherwise fix the disk |
| Disk too slow / saturated | Volume write latency high, IOPS at ceiling; more workers don't help | Move to faster storage (SSD/provisioned IOPS); reduce series/points written |
| Ingest buffer too small for bursty load | Drops spike in short bursts while `stored` keeps up on average | Raise `-ingest-buffer` to absorb the burst (costs memory — watch [TraceForgeNearMemoryLimit](TraceForgeNearMemoryLimit.md)) |
| Genuine overload / more agents than provisioned | Ingest rate stepped up and stayed; agent count grew | Scale out replicas, or reduce per-agent load (longer agent `-interval`) |
| Validate/enrich starved of CPU | `ingested` keeps up but stages lag; CPU throttled | Give the pod more CPU; workers default to GOMAXPROCS, so raise the quota |

## Mitigate

**Stop the bleeding (next five minutes):**

- Backpressure is self-protective — the server is staying up by declining work,
  and agents are being told cleanly. If drops are a short burst and `stored` is
  keeping up on average, the safe move is to widen the shock absorber: raise
  `-ingest-buffer` and roll. This trades memory for burst tolerance.
- If a single agent or tenant is flooding, throttle it at the source (longer
  `-interval`) rather than absorbing it server-side.

**Fix the cause:**

- If the store stage is the wall and the backend/disk can genuinely take
  concurrent writers, raise `-store-workers`. Do this deliberately — the default
  of 1 exists because the backends serialise, and extra workers on a serialising
  backend just queue.
- If the disk is the wall, no worker count fixes it; move to faster storage or
  cut the write volume (fewer series).
- If it is sustained growth, scale replicas out.

## Escalate

Escalate to the service owner when drops persist after buffer and worker tuning,
when the disk is the proven bottleneck and cannot be upgraded quickly, or when
capacity planning is needed. Gather: the dropped-vs-stored-vs-ingested rates
above, `metricsctl --server http://SERVER:8080 stats`, the current
`-ingest-buffer` / `-store-workers` / `-storage` values, and the volume's write
latency.

## Why this alert exists

Dropping is not, by itself, a catastrophe — it is the system protecting itself,
and it is honest about it. But sustained dropping means agents are being turned
away for ten minutes straight, which is a capacity problem that will not fix
itself, and left alone it hides a slow slide toward an overload that *does* start
losing data. The `> 1`/`10m` shape ignores the odd burst and fires only on a real
trend. Keeping it separate from the `failed` alert is the point: they look
similar on a dashboard and demand opposite responses.
