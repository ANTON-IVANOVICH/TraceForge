# TraceForgeHighErrorRate

## What fired

```promql
sum(rate(traceforge_http_requests_total{status=~"5.."}[5m]))
  /
sum(rate(traceforge_http_requests_total[5m]))
  > 0.05
```

More than 5% of HTTP requests to the API returned a 5xx status over the last
five minutes. This is the server telling clients it failed — a genuine
server-side error, not backpressure.

## Impact

Requests that got a 5xx did not do what the caller asked. For a POST to
`/api/v1/metrics` that means a batch the agent will treat as a delivery failure
and (depending on `-http-retries`) retry — so unlike a silent storage failure,
these are at least visible to the sender. For `/api/v1/query` and the dashboard
it means broken reads: alerts evaluated against this server may go stale, and
operators see errors instead of graphs. A sustained 5xx ratio usually means one
route or one dependency is broken for everyone hitting it.

Note what is **not** here: a **429** is not a 5xx. Backpressure — the pipeline
refusing a batch when the ingest buffer is full — returns 429 and has its own
alert, [TraceForgePipelineDropping](TraceForgePipelineDropping.md). This alert
is strictly `status=~"5.."`, so a server shedding load cleanly does not trip it.
If this fires, something is actually erroring, not merely full.

## Diagnose

1. **Which route, and which status?** The discriminating query — split the 5xx
   rate by route and code so you are not debugging the whole server:

   ```promql
   sum by (route, status) (rate(traceforge_http_requests_total{status=~"5.."}[5m]))
   ```

   `route` is the mux's registered *pattern* (`/api/v1/metrics`,
   `/api/v1/query`, `/ws`, …), never the raw path. Unmatched requests — 404s,
   405s, redirects — collapse into `route="other"`; a spike there is malformed
   or misrouted traffic, not a handler bug. A 500 concentrated on one real route
   points straight at that handler.

2. **500 vs 503.** A `500` from the API is almost always a **panic** turned into
   a response by the `Recover` middleware — check the logs for the recovered
   panic and its stack:

   ```sh
   kubectl logs POD --tail=300 | grep -iE 'panic|recovered|runtime error'
   ```

   A `503` is the server refusing a request on purpose — most often
   `/api/v1/metrics` while shutting down (readiness gate closed during a drain),
   or a handler-level dependency being unavailable. If the 503s line up with a
   rollout, it is drain behaviour, not a bug.

3. **Is it one replica or all of them?**

   ```promql
   sum by (instance) (rate(traceforge_http_requests_total{status=~"5.."}[5m]))
   ```

   One instance points at that pod (a bad node, a corrupt local volume, a
   partial deploy). All of them points at a shared cause: a bad release, a
   downstream dependency, or a specific input that panics every replica.

4. **Correlate with a deploy.** Check the rollout history —
   `kubectl rollout history deployment/traceforge-server` — and whether the 5xx
   onset matches the last image change. A panic that only some inputs trigger
   often ships in a release and waits for the right request.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Handler panic (bad input, nil deref, regression) | 500s on one route; logs show a recovered panic + stack; onset matches a deploy | Roll back the release; fix the panic; add the offending input to tests |
| Storage read errors on `/api/v1/query` | 5xx concentrated on `/api/v1/query`; store logs show read/mmap errors | Fix the backend (see [TraceForgeStorageWritesFailing](TraceForgeStorageWritesFailing.md)); a failing disk breaks reads too |
| Draining replica still receiving traffic | 503s on `/api/v1/metrics` during a rollout; readiness gate closed | Expected during drain; ensure `-shutdown-delay` covers the LB's deregistration lag |
| Auth/dependency misconfig (e.g. unreachable JWKS) | 5xx on authenticated routes; logs show token verification failing | Fix `-jwks-url` reachability / key config |
| Attack or misrouted flood | Spike in `route="other"` with 4xx/5xx; unfamiliar paths | Handle at the ingress/WAF; it is not a handler bug |

## Mitigate

**Stop the bleeding (next five minutes):**

- If the onset matches a deploy and one release is the obvious cause, **roll
  back**: `kubectl rollout undo deployment/traceforge-server`. A panic loop does
  not fix itself, and rollback is faster than a root-cause.
- If it is one bad replica, evict it: `kubectl delete pod POD`, having confirmed
  the Service still has healthy endpoints so the rest keep serving.
- If the 5xx are 503s from an in-progress rollout, slow or pause the rollout and
  let each pod finish its readiness/drain cycle before the next.

**Fix the cause:** find the panic in the logs and fix the handler, restore the
failing dependency, or repair storage per the linked runbook. Confirm recovery:
the 5xx ratio query returns below 0.05 and stays there.

## Escalate

Escalate to the service owner when a rollback does not clear it (the cause
predates the last release), when every replica is erroring on a route you cannot
explain, or when the trigger is a specific input you cannot reproduce safely in
prod. Gather first: the per-route/per-status breakdown, the per-instance
breakdown, the recovered-panic log lines with stacks, the rollout history, and
whether a rollback was attempted and what it did.

## Why this alert exists

The RED signals — Rate, Errors, Duration — are how you answer "is the API
healthy?" without reading every log line, and Errors is the sharpest of the
three. The ratio is deliberately computed over `status=~"5.."` only, because
folding 429s in would make honest backpressure look like a server fault and mask
the real thing this watches: the server actually failing. And the metrics
middleware sits **outside** `Recover` in the chain on purpose — `Recover` turns a
panic into a 500, so placing metrics outside it means a panicking request is
still counted here. Move metrics inside `Recover` and every panic would vanish
from this ratio, which is the one number that would have caught it.
