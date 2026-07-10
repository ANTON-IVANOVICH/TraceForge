# TraceForgeAgentStale

## What fired

```promql
time() - traceforge_agent_last_collect_timestamp_seconds > 120
```

The gauge `traceforge_agent_last_collect_timestamp_seconds` holds the wall-clock
moment of the agent's last *successful* collection. Subtracting it from `time()`
gives the age of the freshest metric this agent has produced; over 120 seconds
(sustained 5m) means the agent has gone at least two minutes without a good tick,
even though it is still up and being scraped.

## Impact

A live process that has stopped doing its job. The agent is up — Prometheus is
scraping it, `/healthz` is 200 — but no tick has produced a metric in over two
minutes, so the telemetry for this host has frozen at its last good value.
Downstream, this host looks like it stopped changing rather than stopped
reporting, which is worse: a graph that flatlines is easy to misread as "stable".
On a DaemonSet it is one node whose numbers are stale while the pod looks fine.

The cause is either "every collector is now failing" (the agent still ticks but
each tick errors — see
[TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md)) or "the
tick loop itself has stopped or wedged". This alert catches both, because both
freeze the timestamp.

## Diagnose

1. **How stale, and which agent?**

   ```promql
   time() - traceforge_agent_last_collect_timestamp_seconds
   ```

2. **A subtlety that saves you from a false read.** The agent **omits this gauge
   entirely** until its *first* successful collection — it is absent, not zero,
   on a freshly started agent that has never worked. That is deliberate: a zero
   would be `time() - 0`, an enormous age, and would read as "last collected at
   the Unix epoch, 1970" — indistinguishable from a real staleness. By leaving
   the series absent, a never-worked agent is caught by `absent()` rather than
   masquerading as an ancient-but-real one. So check which case you are in:

   ```promql
   absent(traceforge_agent_last_collect_timestamp_seconds{instance="AGENT"})
   ```

   - `absent()` true → the agent has **never** had a successful collection since
     start. It came up broken; go to
     [TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md).
   - Series present but stale → the agent *was* working and **stopped**. Continue
     below.

3. **Is it still ticking, or has the loop stopped?** This is the discriminating
   query — separate "ticks but every collection fails" from "not ticking at all":

   ```promql
   rate(traceforge_agent_ticks_total{instance="AGENT"}[5m])
   rate(traceforge_agent_collect_failures_total{instance="AGENT"}[5m])
   ```

   - Ticks still advancing, failures climbing → the loop runs but collections
     error. This is collector failure; work
     [TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md).
   - Ticks flat (not advancing) → the tick loop is stuck or stopped: a wedged
     collector blocking the goroutine, a paused process, or clock trouble.

4. **Read the logs and check the clock.**

   ```sh
   kubectl logs POD --tail=200 | grep -iE 'collect|tick|warn|error'
   ```

   The agent logs `agent started` with its `interval` on boot and `warn` lines
   for failures. Note that the alert compares the agent's timestamp against the
   *Prometheus server's* `time()`; a large clock skew between the agent host and
   Prometheus can distort the age, so if the number looks impossible, check NTP
   on the node.

5. **Cross-check the agent's own readiness.** The `collectors` readiness check
   returns `last successful collection was <age> ago` once the age exceeds three
   `-interval`s. If you can reach the telemetry port:

   ```sh
   curl -sS -m 5 http://AGENT:9101/readyz
   ```

   Its body names exactly why it considers itself unready.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| All collectors now failing | Ticks advancing, failures climbing; gauge frozen | Fix the collector — see [TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md) |
| Tick loop wedged / blocked collector | `traceforge_agent_ticks_total` rate is 0; no new failures either | Restart the agent (`kubectl delete pod POD`); capture logs first for the wedge |
| `-interval` longer than the 120s window | Age oscillates just over 120s; `-interval` is large | Expected if interval > ~1m; align the alert window with the configured interval |
| Clock skew between node and Prometheus | Age implausible; NTP off on the node | Fix NTP on the host; the timestamp is real, the subtraction is skewed |
| Never worked since start | `absent()` true; agent just deployed and broken | Go to [TraceForgeAgentCollectorsFailing](TraceForgeAgentCollectorsFailing.md) |

## Mitigate

**Stop the bleeding (next five minutes):**

- If the tick loop is wedged (ticks not advancing), **restart the agent** —
  `kubectl delete pod POD` on a DaemonSet reschedules it — after grabbing the
  current logs, because a restart erases the evidence of the wedge.
- If it is collector failure (ticks advancing, failures climbing), do not
  restart blindly; the restart will hit the same broken dependency. Fix the
  collector per the linked runbook.
- Treat the node as currently unmonitored until fresh metrics resume; do not
  trust the flatlined values.

**Fix the cause:** repair the failing collector, unwedge/restart the loop, fix
node NTP, or align the alert window with a long `-interval` — per the table.
Confirm recovery: `time() - traceforge_agent_last_collect_timestamp_seconds`
drops back under 120 and the gauge advances every interval.

## Escalate

Escalate to the platform/DaemonSet owner when the tick loop wedges repeatedly
after restarts (a genuine hang worth a stack dump), when it is fleet-wide (a bad
DaemonSet rollout froze every agent), or when clock skew is a cluster-wide NTP
problem. Gather first: the staleness age, the `absent()` result, the ticks-vs-
failures rates, the `/readyz` body, and the agent logs around when the timestamp
stopped advancing.

## Why this alert exists

An agent that is up but not collecting is invisible to every up/down check — the
process answers, the scrape succeeds, and the last good value just sits there
looking like a stable metric. This alert watches freshness instead of liveness,
which is the only thing that catches a frozen agent. The gauge is deliberately
*absent* until the first success rather than initialised to zero, precisely so a
brand-new broken agent is caught by `absent()` and cannot hide as a metric that
claims it last collected at the epoch — a zero here would be a lie that reads as
fifty years of staleness.
