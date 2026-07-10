# TraceForge Helm chart

Deploys TraceForge — a Prometheus-like metrics system — as a single-node
**server** (StatefulSet) plus a per-node collection **agent** (DaemonSet), with
the hardened security posture the distroless images were built for.

- Chart version: `0.11.0`
- App version (default image tag): `0.11.0`

## Not highly available — on purpose

The server is single-node: no replication, no consensus. `server.replicas` is
`1` in the defaults **and** in `values-prod.yaml`, and it must stay there.
`replicas > 1` gives you N independent stores with nothing copying data between
them — agents shard across them at random and every query sees a fraction of the
data. That is data loss dressed as scale-out. Horizontal scaling waits for the
clustering stage. See the long comment in `values-prod.yaml`.

## Install

```sh
# defaults (single node, tsdb, locked down, no auth, no network policy)
helm install tf deploy/charts/traceforge

# local / laptop: ephemeral memory store, tiny resources
helm install tf deploy/charts/traceforge -f deploy/charts/traceforge/values-dev.yaml

# production: tsdb + drain window + network policy + ServiceMonitor
helm install tf deploy/charts/traceforge -f deploy/charts/traceforge/values-prod.yaml
```

## Security posture

- Pods run as the distroless nonroot uid/gid `65532`, `runAsNonRoot: true`,
  `fsGroup: 65532`, `seccompProfile: RuntimeDefault`.
- Containers: `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`,
  `capabilities.drop: [ALL]`. `/tmp` is an `emptyDir`; the server also mounts its
  data PVC because a read-only root cannot persist anywhere else.
- **The one exception**: `agent.capture.enabled=true` switches the DaemonSet to
  the `traceforge/agent-pcap` image and runs it as **root** with `NET_RAW` +
  `NET_ADMIN`. A runtime-granted capability only lands in the permitted set of a
  root process, and distroless has no file/ambient capabilities to promote a
  non-root one — so raw-socket capture must be opted into deliberately.

## Secrets

The chart **never** templates a secret value into args or a ConfigMap. Auth
material comes from a pre-created `Secret` via `secretKeyRef`:

- server: `server.auth.existingSecret` (key `server.auth.hs256SecretKey` →
  `JWT_HS256_SECRET`). Enabling `server.auth` without a secret name **fails the
  render**.
- agent: `agent.auth.existingSecret` (keys → `AGENT_API_KEY` / `AGENT_AUTH_TOKEN`).

## Runtime sizing

No `GOMAXPROCS` or `GOMEMLIMIT` env is set. Since Go 1.25 the runtime derives
`GOMAXPROCS` from the cgroup CPU quota and keeps updating it; the binary derives
its soft memory limit from the cgroup via `-memory-limit-ratio`. Setting either
env var would freeze that and mis-size the runtime after a vertical resize.

## Graceful shutdown

`terminationGracePeriodSeconds = shutdown.timeoutSeconds + shutdown.graceBufferSeconds`,
and `shutdown.delaySeconds < shutdown.timeoutSeconds` (both enforced at render
time). There is **no** `preStop` sleep hook: the distroless image has no shell to
run one, and `-shutdown-delay` does the same job inside the process — fail
readiness, let the load balancer drain, then close listeners.

## Verify

```sh
helm lint deploy/charts/traceforge
helm template tf deploy/charts/traceforge | kubeconform -strict -summary -ignore-missing-schemas -
helm template tf deploy/charts/traceforge -f deploy/charts/traceforge/values-prod.yaml | kubeconform -strict -summary -ignore-missing-schemas -
```

## Key values

See `values.yaml` — every key is documented with what it costs to change it.
`values-dev.yaml` and `values-prod.yaml` are ready-made overlays.
