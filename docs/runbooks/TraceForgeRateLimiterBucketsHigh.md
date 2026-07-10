# TraceForgeRateLimiterBucketsHigh

## What fired

```promql
traceforge_ratelimit_buckets > 50000
```

The per-agent rate limiter is tracking more than fifty thousand live token
buckets, sustained for ten minutes. The limiter keeps one bucket per distinct
client key, so this number is "how many distinct agents the server thinks it is
talking to" — and fifty thousand is implausibly many.

## Impact

A slow denial of service wearing traffic's clothes. The limiter keys its buckets
by the **`X-Agent-ID`** request header (falling back to client IP when the header
is absent). A well-behaved fleet has a bounded set of agent ids, so the map is
small and stable. A client that mints a **fresh `X-Agent-ID` per request** — a
bug or an attack — creates a new bucket every time, and the map grows without
bound. That memory is charged to the server, and the sweeping/eviction work it
triggers is CPU charged to every request.

The design keeps this from being fatal, but not free. There is a **10-minute
idle TTL**: a bucket outlives its last request by ten minutes, then a periodic
sweep removes it, so honest churn (agents restarting with new ids, IPs rotating)
costs nothing over time. And there is a **hard cap of 100,000 buckets** with
**batch eviction** of the least-recently-used (it drops a fraction — 1/16 — of
the map at once rather than one bucket per insert, to keep the eviction itself
from becoming the CPU-exhaustion attack). So the limiter degrades rather than
dies. But this alert's threshold of 50,000 sits at half the cap for a reason: as
the map approaches 100,000, eviction starts firing, and eviction drops the
*least recently seen* bucket — which under a flood of fresh ids can be a
**legitimate, slow-reporting agent**. At the cap, real agents start losing their
buckets to the flood. That is the impact: not a crash, but honest agents being
squeezed out by a client abusing the key space.

## Diagnose

1. **Confirm the count and trend.**

   ```promql
   traceforge_ratelimit_buckets
   ```

   A number that climbs steadily toward 100,000 is unbounded key creation. A
   number parked just under 50,000 and flat may just be a large real fleet — size
   it against how many agents you actually run.

2. **Discriminating question: is bucket count tracking real agents, or
   detached from them?** Compare live buckets against the number of agents that
   are actually reporting:

   ```promql
   traceforge_ratelimit_buckets                              # distinct client keys the limiter holds
   count(count by (instance) (traceforge_agent_ticks_total)) # agents actually sending self-metrics
   ```

   If buckets vastly exceed the real agent count, some client is inventing keys —
   this is the abuse case. If they roughly agree, the fleet genuinely grew and
   the threshold (or the cap) needs revisiting, not a culprit.

3. **Find the offender.** The buckets are keyed by `X-Agent-ID`, so the source is
   whatever is sending unique values for it. Look at the ingest access logs for a
   flood of distinct agent ids from one source IP, or an id pattern that looks
   generated (UUID-per-request):

   ```sh
   kubectl logs POD --tail=500 | grep -iE 'ingest|/api/v1/metrics|X-Agent-ID|agent[_-]?id' | head
   ```

   A single client IP behind thousands of distinct ids is the signature.

4. **Is eviction already firing?** If the count is at or near 100,000, the cap is
   active and least-recently-used buckets — possibly real agents — are being
   dropped. Correlate with agents suddenly seeing 429s or send failures
   ([TraceForgeAgentCannotReachServer](TraceForgeAgentCannotReachServer.md)):
   legitimate agents getting throttled unexpectedly is the tell that eviction is
   catching them.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Client generating a fresh `X-Agent-ID` per request (bug) | Buckets >> real agent count; access logs show generated ids from one source | Fix the client to send a stable id; block it until fixed |
| Deliberate key-space flood (DoS) | Torrent of distinct ids from one/few IPs; no matching real agents | Block the source at ingress/WAF; require and validate `X-Agent-ID` upstream |
| Missing `X-Agent-ID`, keying by IP behind a proxy | Many buckets, all fronted by a proxy that varies source IP | Ensure agents send a stable `X-Agent-ID`; fix proxy so the key is stable |
| Genuinely large fleet | Buckets ≈ real agent count; both grew together | Raise the alert threshold to fit the fleet; confirm the 100k cap still has headroom |

## Mitigate

**Stop the bleeding (next five minutes):**

- If one client or IP is minting ids, **cut it off at the edge** — ingress rule,
  network policy, or WAF. Stopping new-key creation lets the 10-minute idle TTL
  drain the map on its own; you do not have to restart anything.
- If eviction is already firing at the cap and real agents are being throttled,
  blocking the abuser is the *only* thing that protects them — adding server
  memory does not, because the cap is a count, not a byte budget.
- Do not raise the 100,000 cap to "make room" for a flood; that just lets the
  attacker consume more memory. The cap protecting you is working as intended.

**Fix the cause:** get the offending client to send a **stable** `X-Agent-ID`
(or block it), or fix the proxy that is destabilising the key. If the culprit is
simply real growth, raise this alert's threshold to match the fleet — but only
after confirming the true agent count, so you are not silencing an attack.
Confirm recovery: `traceforge_ratelimit_buckets` falls back to a number that
matches your real fleet and stops climbing.

## Escalate

Escalate — often to security, not just the service owner — when the id flood
comes from an external or untrusted source (a DoS, not a bug), when legitimate
agents are being evicted at the cap and you cannot identify the offender, or when
the fleet has genuinely outgrown the design and capacity needs rethinking. Gather
first: the bucket-count trend, the buckets-vs-real-agent-count comparison, the
suspect `X-Agent-ID` values and their source IPs from the ingest logs, and
whether eviction (count near 100k) is firing.

## Why this alert exists

An unbounded map keyed by a value the client controls is a classic memory-and-CPU
exhaustion vector, and the ugly part is that it looks exactly like traffic — the
server is "busy", nothing errors, and by the time it hurts, real agents are the
ones being throttled. The limiter's idle TTL and hard-cap-with-batch-eviction
make it degrade gracefully instead of falling over, but "degrade gracefully" is
not "safe": at the cap it starts evicting the honest, slow-reporting agents to
make room for the flood. This alert fires at half the cap so an operator sees the
key-space abuse and blocks the source *before* eviction starts penalising the
agents it is supposed to protect.
