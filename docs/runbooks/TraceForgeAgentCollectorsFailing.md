# TraceForgeAgentCollectorsFailing

## What fired

```promql
rate(traceforge_agent_collect_failures_total[10m]) > 0
```

An agent's collectors are returning errors when the tick fires. Sustained over
fifteen minutes, this means the agent is running and ticking but failing to
gather some or all of the metrics it exists to gather.

## Impact

The agent is a process that looks fine and produces nothing useful. It keeps
running, keeps ticking on its `-interval`, logs the failures at `warn`, and — if
*every* collector fails — sends empty or degraded batches. Nothing turns red on
its own: the process is up, `/healthz` is 200, it is scrapeable. What is lost is
the actual telemetry from this host: the CPU, memory, disk, or network numbers
the agent was deployed to collect are simply missing for the window it is
failing, and no other component fills that gap. On a DaemonSet this is one node
going dark in the fleet while looking healthy.

If *all* collectors fail, this alert has a sibling worth knowing: the agent's own
readiness check reports `every collector has failed since start`, and the
staleness gauge stops advancing — so a total failure also surfaces as
[TraceForgeAgentStale](TraceForgeAgentStale.md). This alert catches the partial
case too, where some collectors work and one is broken.

## Diagnose

1. **Which agent, and how bad?**

   ```promql
   sum by (instance) (rate(traceforge_agent_collect_failures_total[10m]))
   ```

2. **Discriminating query — partial failure or total?** Compare failures against
   successful collections on the same agent:

   ```promql
   rate(traceforge_agent_collect_failures_total{instance="AGENT"}[10m])
   rate(traceforge_agent_collected_metrics_total{instance="AGENT"}[10m])
   ```

   - Collected still climbing alongside failures → **partial**: most collectors
     work, one is broken. Find the one collector.
   - Collected flat at zero while failures climb → **total**: nothing is being
     gathered; the agent is up but blind. Also check
     [TraceForgeAgentStale](TraceForgeAgentStale.md).

3. **Read the warnings — they name the collector.** The agent logs each failure
   at `warn` with the collector and the underlying error:

   ```sh
   kubectl logs POD --tail=200 | grep -iE 'collect|collector|warn'
   ```

   The error text tells you which collector and why: a bad `-disk-path`, a
   `/proc` that is not mounted, or a network-capture failure.

4. **Match the failure to a collector's dependency.**
   - **Disk** collector reads the path in `-disk-path`. If that path does not
     exist in the container's mount namespace, every disk collection errors.
     Confirm the flag and that the path is actually mounted into the pod.
   - **CPU / memory** collectors read the host `/proc` and `/sys`. On a
     containerised agent these must be mounted in; a missing or restricted
     `/proc` mount fails them.
   - **Network** collector (only when `-network` is on) uses libpcap. This is the
     `traceforge/agent-pcap` image, which runs as **root** because opening a raw
     socket needs `CAP_NET_RAW`, and a runtime-granted capability only lands in
     the permitted set of a root process. If capture was enabled on the plain
     `traceforge/agent` image, or the pod lacks `CAP_NET_RAW`, the network
     collector fails every tick. Check `-network`, `-network-device` /
     `-network-file`, and the pod's security context.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| `-disk-path` points at a path not in the container | Logs name the disk collector; path not mounted | Point `-disk-path` at a mounted path, or mount the intended volume |
| `/proc` or `/sys` not mounted / restricted | CPU/memory collectors error; host paths absent in the pod | Mount the host `/proc`,`/sys` (or relax the restriction) as the deployment intends |
| Network capture without privileges | `-network` on, but wrong image or no `CAP_NET_RAW` | Use `traceforge/agent-pcap` and grant `CAP_NET_RAW` (it runs as root by design); or disable `-network` |
| Wrong capture source | `-network-device` names a NIC that isn't there, or `-network-file` path missing | Set a device that exists (`en0`,`eth0`,`any`) or a real `.pcap` for `-network-file` |
| Transient host condition | Failures brief, then clear; error text is a temporary I/O issue | Often self-resolves; investigate the host if it recurs |

## Mitigate

**Stop the bleeding (next five minutes):**

- If the failing collector is one you do not need on this host (e.g. `-network`
  capture that was enabled by mistake), **turn it off** and roll the agent so the
  remaining collectors report cleanly rather than the whole tick being noisy.
- If a mount is missing (`-disk-path`, `/proc`), fix the mount / flag in the
  DaemonSet and roll — the collector cannot succeed until its dependency is
  present.
- If it is a total failure and the node's telemetry matters, treat the node as
  currently unmonitored and lean on host-level monitoring until the agent is
  restored.

**Fix the cause:** correct the flag or mount, switch to `traceforge/agent-pcap`
with `CAP_NET_RAW` for capture, or point the collector at a source that exists —
per the table. Confirm recovery: `rate(traceforge_agent_collect_failures_total)`
returns to 0 and `traceforge_agent_collected_metrics_total` resumes climbing.

## Escalate

Escalate to the platform/DaemonSet owner when the failure is a fleet-wide
misconfiguration (same collector failing on every node — a bad DaemonSet spec),
when capture needs privileges the cluster policy will not grant, or when the
error text points at a host-level fault you cannot fix from the agent. Gather
first: the per-instance failure rate, the partial-vs-total comparison, the `warn`
log lines naming the collector and error, and the agent's flags (`-disk-path`,
`-network*`) and the pod's image and security context.

## Why this alert exists

A collector failure is the quietest way for an agent to become useless: the
process stays up, the probe stays green, the logs scroll `warn` that nobody
reads, and the metrics just aren't there. Because nothing crashes, no other
signal catches it — so this alert watches the one counter that increments when a
collection errors. It is set on a `rate > 0` over ten minutes with a fifteen-
minute `for` so a single transient hiccup does not page, but a collector that is
genuinely broken — a bad mount, a missing capability — does.
