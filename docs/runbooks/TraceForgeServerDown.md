# TraceForgeServerDown

## What fired

```promql
up{job="traceforge-server"} == 0
```

Prometheus has failed to scrape a server's telemetry port for two minutes
straight. `up` is the scrape's own verdict, not a metric the server exports — so
this fires whenever the scrape connection is refused, times out, or returns
non-200, regardless of what the server itself is doing.

## Impact

Possibly none to ingest, possibly total. The telemetry port (`-telemetry-addr`,
default `:9091`) is a **different listener** from the API port (`-addr`,
`:8080`) and the gRPC port (`-grpc-addr`, `:9090`). The server can be accepting
and storing metrics perfectly while its telemetry listener is unreachable, and
it can be failing every ingest while telemetry answers fine. Until you know
which listener is down, you do not know whether you have a monitoring blind spot
or an outage. What is certainly lost while this fires is visibility: every other
TraceForge alert is computed from `/metrics` on this same port, so they go blind
too.

## Diagnose

1. **Tell the two listeners apart — this is the whole job.** From a pod that can
   reach the server (or via `kubectl port-forward`):

   ```sh
   # Telemetry listener — the one Prometheus scrapes.
   curl -sS -m 5 http://SERVER:9091/healthz   ; echo " <- liveness"
   curl -sS -m 5 http://SERVER:9091/readyz     ; echo " <- readiness"
   # API listener — the one agents ingest to.
   curl -sS -m 5 -o /dev/null -w '%{http_code}\n' \
     -X POST http://SERVER:8080/api/v1/metrics -d '{}'
   ```

   - Telemetry refused/timing out **but** the API answering → the process is
     alive and serving; the telemetry listener or its network path is the
     problem. Ingest is fine; this is a monitoring outage.
   - Both refused → the process is down or the pod is gone. Real outage.
   - Telemetry answering → Prometheus's path to it is the problem (network
     policy, wrong target, DNS), not the server.

2. **Is the pod even there?**

   ```sh
   kubectl get pods -l app=traceforge-server -o wide
   kubectl describe pod POD | sed -n '/Events/,$p'
   ```

   Look for `CrashLoopBackOff`, `OOMKilled`, `Evicted`, or a failing
   `Readiness`/`Liveness` probe. The container `HEALTHCHECK` runs the binary's
   `-health-check` flag, which probes `/readyz` on the telemetry port — so a
   telemetry listener that is down will also fail the kubelet's probe and
   restart the pod.

3. **Read the last words.**

   ```sh
   kubectl logs POD --previous --tail=50
   kubectl logs POD --tail=50
   ```

   The server logs a JSON line `"server started"` with the bound addresses once
   it is serving. If you never see it, it died during startup —
   `open storage failed`, `http listen failed`, `telemetry listen failed` are
   the fatal ones, each printed just before `os.Exit(1)`.

4. **Discriminating query — is it one replica or the fleet?**

   ```promql
   count(up{job="traceforge-server"} == 0)
     /
   count(up{job="traceforge-server"})
   ```

   A single replica points at that pod/node. All of them at once points away
   from the server — a Prometheus, network-policy, or service-discovery fault.

## Likely causes

| Cause | How to confirm | Fix |
|-------|----------------|-----|
| Pod OOM-killed or crash-looping | `kubectl describe pod` shows `OOMKilled`/`CrashLoopBackOff`; `--previous` logs show the panic or `exit(1)` | Address the crash; if OOM, see [TraceForgeNearMemoryLimit](TraceForgeNearMemoryLimit.md) and raise the memory limit |
| Startup aborted (bad `-storage`, `-data-dir` unwritable, port in use) | Logs show `open storage failed` / `http listen failed` / `telemetry listen failed` and no `server started` line | Fix the flag/mount and roll the pod |
| Telemetry listener disabled or firewalled while API runs | Step 1: API answers, `:9091` refused; check `-telemetry-addr` is not `""` and the NetworkPolicy allows Prometheus | Re-enable `-telemetry-addr`; open the port to Prometheus |
| Node gone / pod evicted | `kubectl get pods -o wide` shows `Unknown`/`Evicted`, node `NotReady` | Let the scheduler replace it; cordon the bad node |
| Prometheus can't route to a healthy server | All replicas `up==0` but curl from a peer works; check SD targets and NetworkPolicy | Fix scrape config / NetworkPolicy; not a server issue |

## Mitigate

**Stop the bleeding (next five minutes):**

- If it is one crash-looping replica and you have others, confirm the Service
  still has healthy endpoints (`kubectl get endpoints traceforge-server`) so
  ingest keeps flowing, then delete the bad pod to force a clean restart:
  `kubectl delete pod POD`.
- If step 1 proved ingest is fine and only telemetry is unreachable, you are not
  in an ingest outage. Silence downstream TraceForge alerts that are firing only
  because `/metrics` went dark, and treat this as a monitoring fix at normal
  urgency.
- If the whole fleet is down, check the persistent volume / storage backend
  first — a detached PVC or read-only disk takes every replica out at startup.

**Fix the cause:** address whatever `describe`/`logs` surfaced — memory limit,
storage mount, listen address, or the Prometheus-side path.

## Escalate

Escalate to the service owner when replicas keep crash-looping after a restart,
when storage will not open, or when the whole fleet is down and it is not a
Prometheus-side fault. Gather before paging: `kubectl describe pod`, both
current and `--previous` logs, the discriminating count query above, the output
of the step-1 curls, and `kubectl get endpoints traceforge-server`.

## Why this alert exists

`up == 0` is the one signal that survives when the server cannot speak for
itself: it is Prometheus's own record of the scrape, so it fires even when every
exported metric is unavailable. The two-minute `for` rides out a rollout or a
single missed scrape without paging. And step one exists because the API and
telemetry are separate listeners on purpose — conflating them would send people
hunting an ingest outage that isn't there, or missing one that is.
