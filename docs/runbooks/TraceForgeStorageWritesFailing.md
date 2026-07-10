# TraceForgeStorageWritesFailing

## What fired

```promql
rate(traceforge_pipeline_failed_total[5m]) > 0
```

Metrics that were accepted into the pipeline and passed validation are failing
on the storage write. `failed` means exactly one thing: the write to the backend
errored **after** the caller was already told 202 Accepted.

## Impact

This is the quietest data loss the system can produce, and the most important
alert in the set. The pipeline answers at the door: an agent's batch is accepted
with **202** the moment it enters the ingest buffer, long before the store stage
runs. When the store then fails, the metric is gone and **nothing retries it** —
the agent believes it succeeded, moved on, and will never resend. There is no
502 in anyone's logs, no client-side error, no gap an agent will backfill. The
counter `traceforge_pipeline_failed_total` is the *only* trace that anything was
lost.

The pipeline's accounting identity is how you quantify the loss. Once drained:

```
ingested == stored + invalid + failed
```

Every metric that entered reached exactly one terminal state. `failed` is the
slice that vanished after being promised delivery. (`dropped` is not in this
identity — a dropped metric was refused at the door with a 429 and never
ingested; that is [TraceForgePipelineDropping](TraceForgePipelineDropping.md),
and nobody was lied to.)

## Diagnose

1. **Confirm the rate and which replica.**

   ```promql
   sum by (instance) (rate(traceforge_pipeline_failed_total[5m]))
   ```

2. **Read the accounting directly.** `metricsctl` prints the live counters:

   ```sh
   metricsctl --server http://SERVER:8080 stats
   # columns: ingested stored dropped invalid series points
   ```

   There is no `failed` column: derive it from the drain identity,
   `failed = ingested - stored - invalid`. A `failed` that is non-zero and
   growing between two reads while `ingested` keeps rising is active loss. The
   backend errors themselves are logged by the store stage — grep them for the
   root cause:

   ```sh
   kubectl logs POD --tail=200 | grep -iE 'store|write|fsync|wal|bolt|tsdb|disk|read-only|no space'
   ```

3. **Discriminating query — is readiness already catching it?** For the `tsdb`
   backend, the readiness probe's `storage` check calls `TSDB.Ping`, which
   reports the last background fsync error as `wal not syncing: ...`. So a
   replica whose disk has stopped accepting fsyncs fails `/readyz` and is pulled
   from the load balancer on its own:

   ```sh
   curl -sS -m 5 http://SERVER:9091/readyz          # 503 + "wal not syncing" == tsdb fsync is failing
   kubectl get endpoints traceforge-server           # is the failing replica still in rotation?
   ```

   - `/readyz` red on a tsdb replica → durability failure the probe already
     caught; the LB is shedding it. Loss was bounded to writes in flight before
     it left rotation.
   - `/readyz` green but `failed` climbing → the failures are per-write and
     transient-looking to Ping (e.g. bolt/memory backends, or a fault Ping does
     not model). The probe will not save you; act now.

4. **Look at the disk and mount underneath the pod.**

   ```sh
   kubectl exec POD -- df -h /data 2>/dev/null   # if the image had a shell — distroless does not
   kubectl describe pod POD | grep -iE 'volume|mount|readonly'
   kubectl get pvc; kubectl describe pvc DATA_PVC | sed -n '/Events/,$p'
   ```

   (The distroless image has no shell; read the disk from the node or the CSI
   driver's metrics, and the PVC via `describe`.)

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Disk full | Store logs show `no space left on device`; node/PV disk-usage metric at 100% | Grow the PVC/volume; delete old data; then writes resume |
| Filesystem went read-only | Logs show `read-only file system`; `kubectl describe pod` shows the mount `readOnly` or the kernel remounted it after an I/O error | Fix the underlying disk/volume and remount rw; roll the pod onto a healthy node |
| PVC detached / volume lost | Pod events show `FailedMount`/`Multi-Attach`; every write erroring at once | Reattach the volume; if the node is bad, delete the pod so it reschedules |
| tsdb fsync failing | `/readyz` returns `wal not syncing: ...`; replica already out of the LB | Same as read-only/full disk — the durability path is what Ping watches |
| bolt file lock lost | `bolt` backend logs a lock/`timeout` error; another process or a stale mount holds `metrics.bolt` | Ensure a single writer; the bolt file must not be on a shared/NFS mount; restart onto a clean volume |

## Mitigate

**Stop the bleeding (next five minutes):**

- **Give writes somewhere to land.** If the disk is full or read-only, the loss
  continues every second. Free space or reattach a healthy volume immediately;
  that alone stops `failed` from climbing.
- **Get the bad replica out of rotation.** On tsdb this happens automatically
  once Ping reports the fsync error. On other backends, or if `/readyz` is still
  green, take it out yourself: `kubectl delete pod POD` (a peer keeps serving) or
  scale the failing replica down. A replica that answers 202 and then drops the
  write is worse than one that is absent — an absent one at least makes agents
  see send failures and log them.
- Do **not** simply restart into the same broken volume; you will resume losing
  data on the next batch.

**Fix the cause:** grow or replace the volume, clear the read-only condition,
resolve the PVC attach, or move the bolt file off a shared mount — per the table.
Then confirm recovery: `rate(traceforge_pipeline_failed_total[5m])` returns to 0
and `/readyz` is green.

## Escalate

Escalate to the storage/infra owner when the volume cannot be grown or reattached
quickly, when the filesystem keeps remounting read-only (a failing disk), or when
loss is ongoing across multiple replicas. Gather first: the per-instance failed
rate, `metricsctl stats` output, the store-stage error lines from the logs, the
`/readyz` body, and `kubectl describe pvc`. Note the observed loss rate — agents
cannot backfill it, so the owner needs to know how much is gone.

## Why this alert exists

Because the door lies, kindly. Answering 202 the instant a batch is buffered is
what lets the pipeline absorb bursts and keeps agents fast — but it means a
storage failure is completely invisible to the client, and no retry will ever
recover it. `traceforge_pipeline_failed_total` is the single counter that makes
that invisible loss visible, which is why it is `critical` and why deleting it
would leave the most damaging failure mode in the system with no signal at all.
