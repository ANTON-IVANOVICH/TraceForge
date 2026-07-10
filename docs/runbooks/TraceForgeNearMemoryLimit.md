# TraceForgeNearMemoryLimit

## What fired

```promql
traceforge_go_memory_total_bytes
  /
(traceforge_go_memory_limit_bytes < 9e18)
  > 0.9
```

The Go runtime is holding more than 90% of its soft memory limit
(`GOMEMLIMIT`), sustained for fifteen minutes. The `< 9e18` guard drops replicas
where no limit is set — Go reports an effectively-infinite `GOMEMLIMIT` as
`math.MaxInt64`, and dividing by that would make every server look empty.

## Impact

Not death — yet — but a tax. `GOMEMLIMIT` is a **soft** limit: crossing it does
not kill the process, it makes the garbage collector run **harder and more
often** to stay under the line. That CPU comes straight out of request handling,
so the first symptom is usually latency, not a crash — see
[TraceForgeHighLatency](TraceForgeHighLatency.md). The real danger is what sits
just past the soft limit: the **cgroup hard limit**, which is what actually
OOM-kills the container. When that fires the pod dies mid-request, in-flight
ingest is lost, and it restarts cold — showing up as
[TraceForgeServerDown](TraceForgeServerDown.md).

The critical subtlety: **the real headroom is smaller than this ratio suggests.**
The `tsdb` backend mmaps its chunk files, and those mapped pages count against
the **cgroup** but are invisible to `GOMEMLIMIT`, which only governs the Go
heap. So a replica can read as "88% of GOMEMLIMIT" — under this alert's
threshold — while the cgroup is much closer to its hard limit than that number
implies. Treat this alert as conservative on tsdb: you have less room than it
says.

## Diagnose

1. **How close, and which replica?**

   ```promql
   traceforge_go_memory_total_bytes / (traceforge_go_memory_limit_bytes < 9e18)
   ```

2. **Is the GC already paying the tax?** The discriminating query for "soft limit
   is actively costing us" — GC cycle rate climbing as the ratio rises:

   ```promql
   rate(traceforge_go_gc_cycles_total[5m])
   ```

   A GC-cycle rate that climbs in lockstep with the memory ratio means the
   runtime is thrashing to hold the line, and latency is next.

3. **Heap versus mmap — where is the memory?** This is the tsdb trap. Compare
   what Go accounts for against what the cgroup sees:

   ```promql
   traceforge_go_heap_objects_bytes          # live heap objects Go tracks
   traceforge_go_memory_total_bytes           # total Go runtime memory
   ```

   Then read the container's actual RSS / cgroup `memory.current` from the
   node/kubelet metrics. If cgroup usage is well above
   `traceforge_go_memory_total_bytes`, the gap is mmap'd chunk files — real
   memory that `GOMEMLIMIT` cannot see and cannot rein in. On a memory or tsdb
   backend, the fix is fewer series, not GC tuning.

4. **What is driving the heap — series growth?** Storage cardinality is the usual
   engine of memory growth:

   ```promql
   traceforge_storage_series
   traceforge_storage_points
   ```

   A steadily climbing `traceforge_storage_series` is a cardinality problem: some
   client is emitting high-cardinality labels, and every new series is permanent
   memory.

5. **Confirm the limits actually agree.** `-memory-limit-ratio` (default `0.9`)
   sets `GOMEMLIMIT` to that fraction of the detected cgroup limit — *unless*
   `GOMEMLIMIT` was set explicitly in the environment, in which case the flag is
   ignored. Check `traceforge_go_memory_limit_bytes` against the cgroup's hard
   limit: if the ratio is 0.9 there is only 10% of the cgroup between the soft
   limit and the OOM-killer, and mmap eats into even that.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Series cardinality growth | `traceforge_storage_series` climbing steadily; heap tracks it | Find the high-cardinality label source and stop it; drop/relabel offending series |
| mmap'd chunk files (tsdb) filling the cgroup | cgroup `memory.current` >> `traceforge_go_memory_total_bytes`; backend is tsdb | Give the pod more memory (mmap needs real headroom); reduce retained chunks/series |
| `-memory-limit-ratio` too aggressive for a tsdb workload | Ratio at 0.9 but mmap pushes cgroup to the edge; OOM despite GOMEMLIMIT never crossed | Lower `-memory-limit-ratio` so the Go heap leaves room for mmap under the hard limit |
| Large `-ingest-buffer` under burst | Buffer sized big to absorb bursts; memory scales with it | Trim `-ingest-buffer`; trade burst tolerance for headroom |
| Genuine undersizing | Ratio high across replicas at steady state, cardinality stable | Raise the container memory limit; scale out |

## Mitigate

**Stop the bleeding (next five minutes):**

- If a replica is about to OOM (cgroup near the hard limit, GC thrashing),
  **raise the container memory limit** and roll, or scale out so load spreads and
  each replica holds fewer series. More memory is the fastest, safest lever;
  headroom for mmap only comes from real RAM.
- If it is one replica drifting up, evict it (`kubectl delete pod POD`, Service
  endpoints healthy) to reset it while you find the cause — cheaper than an OOM
  mid-request.
- On tsdb, do **not** trust the 90% number as your safety margin; act earlier,
  because the mapped chunk pages are already spending headroom this ratio does
  not show.

**Fix the cause:** stop the cardinality growth at its source, lower
`-memory-limit-ratio` so the Go heap leaves room for mmap under the cgroup hard
limit, trim `-ingest-buffer`, or right-size the memory request/limit. Confirm
recovery: the ratio settles below 0.9 and the GC-cycle rate comes back down.

## Escalate

Escalate to the service owner when replicas keep approaching the limit after more
memory is added (an unbounded cardinality source, or a leak), when tsdb mmap
makes the cgroup OOM despite `GOMEMLIMIT` never being crossed, or when capacity
planning is needed. Gather first: the memory-ratio and GC-cycle queries, the
heap-vs-cgroup gap (with the cgroup `memory.current` reading), the
`traceforge_storage_series` trend, and the current `-memory-limit-ratio`,
`-ingest-buffer`, `-storage`, and container memory limit.

## Why this alert exists

Because "soft limit" lulls people. `GOMEMLIMIT` never kills anything, so it is
tempting to ignore this — but sitting above 90% means the GC is already burning
CPU it would rather spend on requests, and the cgroup's hard limit, which the
soft limit knows nothing about, is not far behind. The alert exists specifically
to buy the fifteen minutes between "the GC is working too hard" and "the kernel
OOM-kills the pod". And it is written to remember the one fact that a naive
memory alert forgets: on tsdb the mmap'd chunk files are counted by the cgroup
and not by `GOMEMLIMIT`, so this number always overstates how much room you have.
