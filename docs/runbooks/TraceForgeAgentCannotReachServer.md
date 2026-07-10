# TraceForgeAgentCannotReachServer

## What fired

```promql
rate(traceforge_agent_send_failures_total[5m]) > 0
```

An agent is collecting metrics fine but failing to deliver its batches to the
server. `send_failures` increments when the transport — HTTP or gRPC — fails to
hand a batch off, after the agent's own retries are exhausted. Sustained over ten
minutes, this means the agent has telemetry and nowhere to put it.

## Impact

The metrics are being gathered and then lost in transit. Where the numbers
actually end up depends on the send path: a batch that exhausts `-http-retries`
is dropped by the agent, so the telemetry for this host is missing for the
window the transport is broken. This is different from a *collection* failure —
the agent knows its data is good, it just cannot deliver it. On a DaemonSet it is
one node whose metrics never reach the server even though the agent is healthy.

**Why the agent's readiness probe stays GREEN while this fires — read this, it is
the whole point of the alert.** The agent is a DaemonSet. Nothing routes traffic
*to* an agent; its readiness gate does not admit or shed request load the way a
server's does. On a DaemonSet, readiness gates **rollouts** — the rolling-update
controller waits for each pod to become ready before moving to the next. The
agent's `collectors` readiness check therefore reports only "is this process
doing its own job?", i.e. "has it collected recently?" — and it **deliberately
ignores whether the server is reachable.**

That omission is intentional and load-bearing. If agent readiness tracked server
reachability, then the moment the server went down, *every* agent would report
not-ready, and a DaemonSet rollout would stall on the first pod — during a server
outage, which is exactly the moment you most need to be able to roll out a fix
(to the agents, to the node, to anything). Tying the two together would turn a
server outage into a fleet-wide deploy freeze. So the agent stays ready and keeps
trying, and this **metric-based** alert — not the probe — is what tells you
delivery is failing. The green probe is not a bug; it is the design refusing to
couple agent rollouts to server health.

## Diagnose

1. **How many agents, and is it one server or the fleet?** The discriminating
   question: a single agent failing is a local network/config problem; *all*
   agents failing at once points at the server or the shared path to it.

   ```promql
   sum by (instance) (rate(traceforge_agent_send_failures_total[5m]))
   count(rate(traceforge_agent_send_failures_total[5m]) > 0)   # how many agents are failing
   ```

   - One or a few agents → local: that node's egress, DNS, or per-agent auth.
   - Every agent at once → the server side: check
     [TraceForgeServerDown](TraceForgeServerDown.md) first — if the server is
     down, this fires everywhere and the fix is there, not here.

2. **Which transport, and to where?** The failure mode differs by `-transport`:
   - `http` → the agent POSTs to `-server` (default `http://localhost:8080`)
     with `-http-timeout`, retrying `-http-retries` times with `-http-backoff`.
   - `grpc` → the agent dials `-grpc-server` (host:port). A wrong target or a
     disabled server gRPC listener (`-grpc-addr` empty) fails every send.

   Read the agent logs for the transport error text:

   ```sh
   kubectl logs POD --tail=200 | grep -iE 'send|transport|http|grpc|deliver|batch|refused|timeout|unauthorized|401|403'
   ```

3. **Prove reachability from the agent's vantage point.** From the agent pod's
   node or a pod on the same network, hit the server's API listener (not the
   telemetry port — agents ingest to `-addr`, `:8080`):

   ```sh
   curl -sS -m 5 -o /dev/null -w '%{http_code}\n' -X POST http://SERVER:8080/api/v1/metrics -d '{}'
   ```

   Connection refused / timeout → network path or the server is down. A `401`/
   `403` → auth, not connectivity (next step). A `429` → the *server* is shedding
   load ([TraceForgePipelineDropping](TraceForgePipelineDropping.md)) and the
   agent is retrying into a full door.

4. **Rule auth in or out.** If the server runs with `-auth`, the agent must
   present a credential: `-api-key` (matched against the server's `-api-keys`
   file) or `-auth-token` (a JWT the server validates via `-jwt-hs256-secret` /
   `-jwks-url`). A rotated key or expired token turns every send into a 401/403
   while the network is perfectly fine — the logs and the curl status tell auth
   apart from connectivity.

5. **Server confirms it is not receiving.** From the server side, its ingest
   counter should be flat for the missing agents while other agents still arrive:

   ```promql
   sum(rate(traceforge_pipeline_ingested_total[5m]))   # is the server receiving anything at all?
   ```

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Server down / not listening on the ingest port | All agents failing; curl to `:8080` refused; server unhealthy | Fix the server — [TraceForgeServerDown](TraceForgeServerDown.md); agents recover on their own |
| Wrong `-server` / `-grpc-server` target | One agent (or a bad rollout) failing; curl works from elsewhere | Correct the target flag and roll the agent |
| gRPC transport but server gRPC disabled | `-transport grpc` and server `-grpc-addr` is empty | Enable gRPC on the server or switch the agent to `-transport http` |
| Auth rejected (rotated key / expired token) | Logs and curl show 401/403; connectivity fine | Update `-api-key` / `-auth-token` to match the server's `-api-keys` / JWT config |
| Network policy / DNS between agent and server | Curl from the node refused/times out; DNS fails | Fix the NetworkPolicy or DNS; open agent→server on the ingest port |
| Server shedding load (429) | Curl returns 429; server pipeline dropping | Not a reachability fault — see [TraceForgePipelineDropping](TraceForgePipelineDropping.md) |

## Mitigate

**Stop the bleeding (next five minutes):**

- If **every** agent is failing, this is almost certainly the server or the
  shared network path — go fix that ([TraceForgeServerDown](TraceForgeServerDown.md)).
  The agents need no action; they keep collecting and will deliver again the
  instant the server returns, because they never stopped trying (that is why
  their readiness stayed green).
- If it is **one** agent, the loss is confined to that node. Correct its target
  or credential and roll just that pod; do not touch the fleet.
- If it is auth (401/403), restore the credential — a rotated `-api-keys` entry
  or expired token — rather than chasing the network.

**Fix the cause:** restore the server, correct the transport target, enable the
matching listener, or refresh the credential — per the table. Confirm recovery:
`rate(traceforge_agent_send_failures_total)` returns to 0 and the server's
`traceforge_pipeline_ingested_total` resumes counting the recovered agents.

## Escalate

Escalate to the service or network owner when every agent is failing and the
server is *not* down (a shared network/DNS/policy fault), when auth is rejecting
agents you cannot re-credential quickly, or when the gRPC path is broken and the
server config is owned elsewhere. Gather first: the per-instance and count
failure queries, the transport error lines from the agent logs, the result of
the curl to the ingest port (with status code), and the agent's `-transport`,
`-server`/`-grpc-server`, and auth flags.

## Why this alert exists

Delivery failure is invisible from the agent's liveness and readiness signals by
design — the process is up, it is collecting, and its probe is deliberately green
because coupling a DaemonSet's readiness to server health would freeze every
agent rollout during a server outage. That leaves a real gap: an agent can be
perfectly healthy and yet silently failing to ship a single metric. This
metric-based alert is what fills that gap, watching the send-failure counter that
the probe was intentionally built to ignore, so that "the agents can't reach the
server" pages someone even though nothing about the agent looks unhealthy.
