# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.11.0] - 2026-07-10

Deployment. The step from "works on my machine" to "an operator can run this, and
when it breaks at 03:00 they have somewhere to look".

The theme of the stage is that a deployment artefact is source code that nothing
compiles. A Kubernetes manifest passing a flag the binary no longer has is
syntactically perfect. A dashboard naming a metric that was renamed shows an empty
graph, which is indistinguishable from a healthy system, and stays that way until
somebody looks at it during an incident. So the artefacts here are all verified —
the images were built and run, the chart was rendered and validated against the
Kubernetes schemas, the exposition format was checked by Prometheus's own
`promtool`, and `test/deploy` is the compiler the YAML otherwise lacks.

### Added

- **Probes done properly** (`internal/server/health`). `/healthz` (liveness)
  depends on nothing — a liveness probe that checks the database restarts the
  whole fleet the minute the database has a bad one. `/readyz` (readiness) checks
  dependencies and carries the drain gate. `/startupz` is a fact about
  initialisation, never a timer. The handlers are O(1): checks run on a background
  prober and publish an immutable snapshot through an `atomic.Pointer`, because a
  probe handler that took the storage lock would turn one slow disk into a
  fleet-wide readiness failure.
- **`Storage.Ping(ctx)`** on the interface, and with it a readiness check that is
  worth having: the TSDB reports its last *background fsync error*.
- **A shutdown in the only order that works.** On SIGTERM the server fails
  readiness first, keeps serving for `-shutdown-delay`, and only then closes its
  listeners. `http.Server.Shutdown` cannot tell the load balancer, and Kubernetes
  removes the endpoint concurrently with the signal; a server that closes its
  listener immediately spends that window refusing connections still being routed
  to it. Three contexts (`sigCtx`, `runCtx`, `telCtx`) keep the phases apart; the
  telemetry listener stops last so the final scrape is not a connection refused.
  `-shutdown-timeout` bounds the whole drain.
- **`/metrics`, written from scratch** (`internal/promexport`): the Prometheus text
  exposition format v0.0.4, with the escaping rules stated, tested, and proven
  invertible by a round-trip fuzz target — the same discipline as the series-key
  encoder. Counters, gauges, cumulative histograms and labelled vectors with a hard
  cardinality cap and a visible `__overflow__` series. Validated in CI by
  `promtool check metrics` against a live scrape.
- **RED metrics for the HTTP API** (`internal/server/httpmetrics.go`). The `route`
  label is the mux's registered *pattern*, obtained from `http.ServeMux.Handler`,
  never `r.URL.Path`; unmatched requests fold into `route="other"`. A hijacked
  connection (the WebSocket) is counted as a 101 but its duration is not observed,
  because a browser tab's lifetime in a latency histogram moves the server's p99 to
  "one hour". The middleware sits *outside* `Recover`, so panicking requests appear
  in the error rate.
- **Self-metrics for both binaries**: pipeline counters, storage size, rate-limiter
  bucket count, active alerts by state and severity, agent collect/send counters,
  and `traceforge_build_info` — a constant-1 gauge whose labels make
  `count by (version) (traceforge_build_info)` answer "how far has the rollout got".
  Runtime metrics come from `runtime/metrics`, not `ReadMemStats`, which stops the
  world; they are namespaced `traceforge_go_*` rather than borrowing
  `client_golang`'s `go_*` names, because a dashboard panel that silently means
  something else is worse than one that shows no data.
- **A telemetry listener** (`internal/telemetry`), separate from the API port and
  from pprof. `/metrics` names every tenant's alert severities; pprof's
  `/debug/pprof/cmdline` prints argv, which is where `-jwt-hs256-secret` lives; and
  a saturated API listener is exactly when the readiness probe most needs to answer.
- **`internal/buildinfo`**: `-ldflags -X` reconciled with `debug.ReadBuildInfo`.
  ldflags wins field by field, except the dirty bit, which is always VCS's — a
  release built from a modified tree is what an operator debugging a bad rollout
  needs to see, and the pipeline believes it is building a clean tag. `-version` on
  every binary; `metricsctl version` now reports the commit and the dirty flag.
- **`internal/container`**: `GOMEMLIMIT` derived from the cgroup memory limit,
  walking the cgroup v2 chain to the root and taking the minimum, because a parent's
  limit binds its children whatever the leaf says. The ratio is a knob and not a
  constant because `GOMEMLIMIT` does not bound `mmap`'d file pages, and the TSDB
  mmaps its chunk files.
- **`-health-check`** on the server and the agent: the binary GETs its own `/readyz`
  and exits 0 or 1. distroless has no shell, so a shell-form `HEALTHCHECK` cannot
  run and there is no `curl` for the exec form to call. The binary was already
  there; it only needed a flag.
- **Images** (`deploy/docker/Dockerfile`): four targets, all distroless and static.
  `server`, `agent` and `metricsctl` are `CGO_ENABLED=0` and cross-compile from
  `$BUILDPLATFORM` in seconds; `agent-pcap` links libpcap 1.10.6 statically against
  musl and — alone among them — runs as root, because a raw socket needs
  `CAP_NET_RAW` and a capability granted by the runtime lands in the permitted set
  of a root process only.
- **`deploy/compose.yaml`**: server + agent, with Prometheus and Grafana behind a
  profile. The Kubernetes manifests are hard to try, and a thing nobody tries is a
  thing nobody trusts.
- **A Helm chart** (`deploy/charts/traceforge`), and `deploy/k8s/traceforge.yaml`
  rendered from it by `make manifests` and committed, so an operator without helm
  can `kubectl apply -f` and so the Go tests can read the exact bytes a cluster
  receives. CI fails on drift. `values-prod.yaml` pins one replica: two would be two
  independent stores with no replication, which is data loss dressed as scale-out.
- **Ten alert rules, three dashboards, ten runbooks** — one per alert, each linked
  from the alert's `runbook_url`, each structured for someone who did not write the
  code and was asleep ninety seconds ago.
- **`test/deploy`**, the compiler the deployment artefacts otherwise lack: every
  `-flag` in a manifest is cross-checked against the flag names parsed out of the
  `cmd/` sources with `go/ast`; every `traceforge_*` token in a dashboard or an alert
  must be a metric a binary really exports; every alert must link a runbook that
  exists and every runbook must be named by exactly one alert; the rendered manifests
  must be hardened; and `shutdown-delay < shutdown-timeout <
  terminationGracePeriodSeconds`.
- **Supply chain**: `.goreleaser.yaml` (archives, SPDX SBOMs, keyless Sigstore
  signing), a `workflow_dispatch`-gated release workflow with a `push` input that
  defaults to false, `govulncheck` and Trivy in CI, and multi-arch image builds. The
  release workflow's tag trigger is committed but commented out: a tag-triggered
  release pushes images to a registry and writes an append-only entry into a public
  transparency log, and nobody typing `git push --tags` has consented to that.

### Fixed

- **The TSDB's background `fsync` error was logged and dropped.** `syncLoop` called
  `wal.Sync()`, wrote the failure to the log, and continued. Meanwhile `Write`
  returned nil (the bytes were in the page cache), the head served them back on
  query, and nothing anywhere was red. On a full disk this is the worst state a
  durable store can be in: it looks healthy, it answers every request, and it is
  keeping nothing. The error is now published and `Ping` reports it, so the replica
  fails readiness and leaves the load balancer. A regression test breaks the WAL's
  file descriptor underneath the sync loop and asserts `Ping` notices while `Query`
  still succeeds — because that combination is the whole point.
- **The pipeline sized its worker pools from `runtime.NumCPU()`.** That number is
  the machine's cores narrowed by the affinity mask; it knows nothing about a cgroup
  CPU *quota*. On an eight-core host with `--cpus=1.5` the server started eight
  validate and eight enrich workers to share one and a half cores of quota. They are
  sized from `GOMAXPROCS` now, which the Go runtime derives from the quota — and
  keeps re-deriving, which is precisely why this project never sets it.
- **Eight reachable vulnerabilities**, found by `govulncheck`: six in the standard
  library (`crypto/tls`, `crypto/x509`, `net/http`, `net/textproto`, `html/template`,
  `net`), fixed by pinning `toolchain go1.26.5`; and two in dependencies, fixed by
  `google.golang.org/grpc` v1.79.3 and `golang.org/x/net` v0.53.0. The scan now
  reports zero reachable.

### Fixed (found by the stage's own adversarial review)

Four reviewers, then two skeptics per finding whose job was to refute it. Nine
survived.

- **The release workflow could not have released anything.** Both hand-assembled
  image references — the one `cosign` signs and the one Trivy scans — interpolated
  `github.repository_owner` raw. An OCI repository name may not contain an
  uppercase letter, and this owner login does; `docker/metadata-action` silently
  lowercases what it is given, so the *build and push* worked while both steps that
  spelled the name themselves would have failed. Trivy's reference was doubly
  wrong: `{{version}}` strips the leading `v`, so `:v0.11.0` was a tag no push ever
  created. The name is now computed once, lowercased, and shared.
- **A capture agent's ingest would have been silently dropped.** Packet capture
  needs `hostNetwork` — a raw socket in the pod's own namespace sees a veth and
  nothing else — and a `hostNetwork` pod's packets carry the node's address and none
  of the pod's labels, so the server's NetworkPolicy `podSelector` could never match
  them. Nothing would break loudly: the agent keeps collecting, the server keeps
  serving, the metrics stop arriving. The chart now `fail`s the render unless
  `networkPolicy.nodeCIDRs` is set alongside capture.
- **Every pod would have been scraped twice.** The ServiceMonitor's selector matched
  both server Services — headless and ClusterIP, which share their selector labels
  and both publish the telemetry port — and the `prometheus.io/scrape` annotations
  were emitted unconditionally beside it. The annotations are now mutually exclusive
  with the ServiceMonitor, and exactly one Service carries `traceforge.io/scrape`.
- **`TestShutdownBudgetsAreConsistent` could check nothing and pass.** Deleting
  `-shutdown-timeout` from the manifest made `argDuration` return 0, the loop
  `continue`d past every container, and both ordering assertions were never reached.
  Every sibling test in that file has a "this test is checking nothing" floor; this
  one now does too.
- **Three comments stated things that are false**, which this project treats as
  defects rather than as prose:
  - The Makefile claimed the Go toolchain stamps VCS metadata into "any binary built
    inside a git tree, which is why `go run` and `go test` need nothing". They are
    not stamped: `go test -c` produces a binary with no `vcs.*` settings, and
    `go run ./cmd/metricsctl version` prints `dev (unknown, unknown, …)`.
  - `httpmetrics.go` claimed that "404s, 405s and redirects have no pattern".
    Redirects do: `ServeMux` answers `GET /foo` with a 307 to `/foo/` and reports
    the *registered* pattern. Harmless for cardinality, wrong as written, and now
    covered by a test that reads the status rather than asserting it.
  - `internal/container` described the runtime's GOMAXPROCS derivation as a plain
    minimum. It is `min(affinity CPUs, max(ceil(quota), 2))` — a fractional quota
    rounds up and a sub-two-core quota is floored at two. `--cpus=1` on an
    eight-core host gives GOMAXPROCS=2, not 1.
- **The container images stamped a build date unrelated to the commit.**
  `github.event.repository.updated_at` changes when somebody edits the repository
  description; the archives already used `.CommitDate`. The same tag rebuilt later
  produced different bytes. The date now comes from the tagged commit.
- Workflow inputs are passed through `env:` rather than interpolated into `run:`,
  where `${{ }}` is textual substitution the shell never sees as data.

### Changed

- `Handler.Routes()` returns `*http.ServeMux` rather than `http.Handler`, because
  the metrics middleware has to ask the mux which pattern a request matched.
- `cli.NewRootCmd` takes a `buildinfo.Info` instead of a version string, so
  `metricsctl version -o json` reports the commit, the build date and the dirty flag.
- The Makefile's `-ldflags` now target `internal/buildinfo` rather than
  `main.version`, so every binary reports its build the same way.
- `-validate-workers` and `-enrich-workers` default to `GOMAXPROCS` rather than
  `NumCPU`.

### Notes

- `-telemetry-addr` defaults to `:9091` and is therefore **on**, unlike every other
  subsystem this project has added. Auth, alerting, packet capture and pprof all cost
  something or expose something. A liveness probe costs nothing, and a probe you have
  to remember to enable is a probe that is missing on the day it matters. `/metrics`
  does expose the process's internals — which is why it shares a listener you can
  firewall, rather than sitting on the API port.
- `GOMAXPROCS` is never set, and this is the one piece of common Helm-chart advice
  the chart deliberately contradicts. Since Go 1.25 the runtime derives it from the
  cgroup CPU quota and re-derives it as the quota changes; setting the environment
  variable through the downward API freezes that. So does calling
  `runtime.GOMAXPROCS(n)` with any positive `n` — including
  `runtime.GOMAXPROCS(runtime.NumCPU())`, which looks like a no-op and is not.
  `internal/container` reads the value from `runtime/metrics`, which has no setter,
  so the mistake is not merely discouraged but unrepresentable.
- Distributed tracing is documented as absent rather than shipped. The interesting
  part is W3C trace-context propagation and sampling, not the exporter — and an
  exporter with no collector to send to would be code nobody has run, which is what
  the previous stage's discipline was about.

### Dependencies

- **No new modules.** `prometheus/client_golang` would have added a dependency tree
  to emit a text format whose entire content is a few hundred lines of escaping
  rules — rules this project can state, test and prove invertible itself, exactly as
  it did for the series-key encoder, the WAL and the libpcap binding.
- `google.golang.org/grpc` v1.76.0 → v1.79.3 and `golang.org/x/net` v0.42.0 → v0.53.0
  (both `govulncheck`-reachable). `toolchain go1.26.5` pinned in `go.mod`.

## [0.10.0] - 2026-07-10

CGo: the project leaves pure Go for the first time, to do something Go cannot —
read packets off a live interface. The stage is as much about the *cost* of that
border crossing as about the crossing itself, and about the discipline of asking
whether it was necessary at all.

### Added

- `internal/agent/network`: a **libpcap binding** for the agent, and with it the
  three hard parts of CGo done properly.
  - **Memory ownership.** Every `C.CString` is paired with a `C.free`. Every
    packet is copied out of libpcap's reusable receive buffer with `C.GoBytes`
    before it is returned — a test deletes that copy and watches the first
    packet turn into the second.
  - **Handle lifetime.** `pcap_close` frees the handle for good, so an
    `RWMutex` guards it: readers hold it while inside C, `Close` takes it for
    writing and therefore cannot free a handle another goroutine is still in.
    `Close` calls `pcap_breakloop` first, so a blocking read is interrupted
    rather than waited on. A `runtime.AddCleanup` (not the legacy
    `SetFinalizer`) is the safety net for a caller who forgets.
  - **C→Go callbacks.** `pcap_loop` calls back into an `//export`ed Go
    function. Handing C a Go pointer to give back later is what the cgo pointer
    rules forbid, so the callback is found through an opaque integer handle —
    `runtime/cgo.Handle`, which is the hand-rolled registry everyone writes,
    already in the standard library. A panic in the callback is recovered at the
    boundary, because unwinding through a C stack frame is undefined behaviour.
- A **C shim** (`pcap_shim.c`/`.h`) rather than C in a Go comment: `struct
  pcap_pkthdr` embeds a platform-dependent `timeval`, libpcap's headers carry
  unions cgo cannot translate, and a file using `//export` may not define C
  functions in its preamble.
- A **link-layer aware packet parser**, pure Go: Ethernet (with stacked VLAN
  tags), BSD loopback (`DLT_NULL`, whose address family is in the *writer's*
  byte order), OpenBSD `DLT_LOOP`, raw IP, and Linux cooked capture. It walks
  IPv6 extension headers, because a packet behind a hop-by-hop header does not
  name its transport protocol in the fixed header — reading it there reports a
  fleet full of protocol 0. Bounded against crafted VLAN and extension chains,
  and fuzzed.
- The **network collector**: a background capture goroutine feeding atomic
  counters, so `Collect` is a snapshot read and never blocks the agent's tick.
  Reports packets, bytes (wire length, not the truncated copy), per-protocol and
  per-IP-version counts, unparsed frames, and — the metric that keeps the others
  honest — **kernel drops** from `pcap_stats`, because under load the kernel
  discards packets before this process ever sees them.
- **`internal/agent/kernel`**: kernel network counters (TCP retransmits, resets,
  listen-queue overflows, UDP errors) read straight from `/proc/net/snmp` and
  `/proc/net/netstat` — **no CGo at all**. It is the stage's counterweight: the
  metrics people reach for eBPF or a libbpf CGo binding to obtain, sitting in two
  text files. Its parser takes an `io.Reader`, so it is tested on a machine with
  no `/proc`.
- **Benchmarks of the boundary itself**, which is the number that decides how a
  binding is shaped:

  | | ns/op |
  | --- | --- |
  | Go call | 0.29 |
  | CGo call | 20.7 |
  | `C.GoBytes` (64 B) | 19.6 |
  | `C.CString` + `free` | 63.9 |
  | pass a Go pointer (64 B) | 26.4 |
  | ...through a `runtime.Pinner` | 57.7 |
  | ...copied with `C.CBytes` | 95.4 |

  A CGo call costs ~70× a Go call, so a binding must cross rarely and do much on
  the far side. `runtime.Pinner` is *not* needed to pass a `[]byte` to a
  synchronous C call — the pointer rules already allow it — and pinning anyway
  doubles the cost. And the callback path measures **faster** per packet than the
  synchronous one (≈128ns vs ≈153ns), because `pcap_loop` crosses into C once and
  amortises it over every packet. The comment that first claimed otherwise was
  corrected by the benchmark.
- **Graceful degradation and the cross-compilation tax.** Capture lives behind
  the `cgo` build tag; `CGO_ENABLED=0` still builds a complete agent that reports
  the collector as unavailable. `make cross-nocgo` cross-compiles to four
  platforms from nothing; `make cross-cgo` fails on purpose, and its error is the
  lesson. New CI jobs build, vet and test both modes — the no-CGo job is what
  caught `ErrTimeout` being declared in the CGo file and used from a shared one.
- Agent flags: `-network` (off by default), `-network-device`, `-network-file`,
  `-network-filter`, `-network-snaplen`. Every failure to open a capture —
  no CGo, no libpcap, no permission, no such interface — is logged once and
  skipped. An agent that will not start because it could not open a raw socket
  reports nothing at all.

### Fixed (found by the stage's own adversarial review)

- **A panicking `Loop` callback never stopped the loop.** The handler recovered
  the panic and recorded an error, but `pcap_loop` kept dispatching: on a
  savefile the file ends and hides it; on a live interface `Loop` blocks forever,
  holding the read lock `Close` needs. The recover now calls `pcap_breakloop`.
  The first regression test for this was itself decorative — it counted callback
  invocations, which are suppressed either way — so the test now measures the
  time the loop takes against a full traversal.
- **`Collect` entered libpcap concurrently with the capture goroutine.** An
  `RWMutex` guards the handle's *lifetime*, but it lets two readers in at once,
  so `Collect`→`pcap_stats` ran while `Run` sat in `pcap_next_ex` on the same,
  non-thread-safe `pcap_t`. A separate mutex now serialises every entry into
  libpcap (except `pcap_breakloop`, which must interrupt rather than wait), and
  the collector samples the drop counters on its own goroutine, so `Collect`
  reads only atomics.
- **`cgo.Handle.Value` panicked outside the recover** — the one call in the
  exported handler able to unwind into a C stack frame was the one not guarded.
- **The `!cgo` stub had no test at all**; `make test-nocgo` was a compile check.
  Its contract is now pinned.
- **`FuzzParse`'s seeds named the wrong link types**: the seed passed
  `int(LinkRaw)`=12, which the modulo mapped onto loopback. The corpus looked
  complete while covering the wrong branches. Seeds now pass indices, and a test
  fails if the table is reordered.
- **The `/proc/net` parser accepted a line naming no protocol** (`":"`),
  producing a section with an empty prefix. Found by a fuzz invariant asserting
  the parser never reports a name or value absent from its input.

### Notes

- **eBPF is documented, not shipped.** `cilium/ebpf` is Linux-only and its
  toolchain needs `clang`, `vmlinux.h` and a kernel to attach to; none of that
  could be compiled or run on the machine this stage was built on. Committing a
  few hundred lines of Linux-only Go and restricted C that has never been through
  a compiler would contradict the discipline established in v0.9.0. The `kernel`
  collector ships instead: it is the same metrics, obtained the way you should
  check for first. `ARCHITECTURE.md` records what the eBPF path would look like
  and when it is worth its price.

## [0.9.0] - 2026-07-09

Testing, benchmarking and profiling raised from "as we went" to an engineering
discipline of its own. The point of the stage is not the count of new tests but
what they *found*: writing an invariant-based fuzzer and a mutation tester turns
up bugs that a green unit suite hides. Several of the fixes below are real
correctness and security defects that existed in shipped code.

### Added

- `internal/testutil`: the shared test infrastructure — object builders, custom
  assertions, an `Eventually`/`Never` poller for asynchronous conditions, a
  golden-file helper with an `-update` flag and output normalisation, and a
  **goroutine-leak detector** (`NoLeaks`) that snapshots goroutine IDs and
  reports any that outlive the test. The detector has its own tests in both
  directions, because a leak detector that silently stops detecting is the worst
  kind.
- **17 fuzz targets**, asserting invariants rather than just "does not panic":
  round-trip and injectivity for the series key; round-trip and idempotency for
  the rule parser and its `String()`; conservation for the chunk codec; the
  prefix property for WAL replay; forgery-resistance for the JWT verifier;
  round-trip for protobuf and JSON metric encoding; distinctness for the alert
  fingerprint. Every discovered crasher is committed under `testdata/fuzz/` as a
  regression seed.
- **A benchmark suite** across the hot paths (series key, storage, pipeline, rule
  evaluation, auth, WAL, chunk, table rendering), with sub-benchmarks over the
  parameters that matter and `b.ReportAllocs` throughout — allocations feed the
  GC, and the GC stops the world.
- `cmd/benchcmp` + `internal/benchcmp`: a from-scratch **benchstat**. It parses
  `go test -bench` output and reports whether a difference is statistically
  significant with a **Mann-Whitney U test** — exact for small samples, a normal
  approximation with a tie and continuity correction otherwise — so noise reads
  as `~` instead of a phantom regression. Every p-value was pinned against SciPy
  and an independent brute-force enumeration in review (agreement < 1e-9 across
  equal/unequal n, ties, and the all-identical `0 allocs/op` case). It warns when
  a file mixes GOMAXPROCS (`-8` vs `-4`) under one benchmark name.
- `cmd/mutate` + `internal/mutate`: a from-scratch **mutation tester**. It splices
  one operator at a time in the AST (`>`→`>=`, `&&`→`||`, `true`→`false`, a
  literal `+1`) by byte offset — so formatting and `//go:build` lines survive —
  and reruns the package's tests against each mutant using `go test -overlay`,
  never copying the tree. Verdicts come from `go test -json`'s structured stream,
  so a mutant whose own output mentions `[build failed]` is not misfiled; a
  cancelled run kills the test binary via its process group rather than orphaning
  it. A surviving mutant is a line the tests run but do not check.
- **The test pyramid, split by build tag.** Unit tests (no tag, `-race`),
  integration tests (`//go:build integration`: real bbolt/tsdb files, `httptest`,
  gRPC over `bufconn`, WAL crash/torn/corrupt recovery, tenant isolation against
  a real store), and an **e2e suite** (`//go:build e2e`, `test/e2e/`) that builds
  the real binaries and runs them as separate processes on `:0` ports, reading
  the bound address back from the server's structured log.
- **`Makefile`**: `test-unit|test-integration|test-e2e|test-all`, `cover`,
  `fuzz`/`fuzz-long`, `bench`/`bench-save`/`bench-compare`, `mutate`, and
  `profile-cpu|heap|trace`.
- **CI** (`.github/workflows`): the pyramid per push (lint, unit+coverage,
  integration, e2e, a 15s fuzz smoke, and an informational `benchcmp` on PRs);
  a separate workflow for 10-minute fuzzing and mutation testing, run on demand
  (`workflow_dispatch`) — its nightly cron is committed but commented out, so a
  fork does not spend Actions minutes unattended.
- A **separate pprof listener** (`-pprof-addr`, off by default) and optional
  mutex/block profile sampling (`-mutex-profile-fraction`, `-block-profile-rate`).
  The server now logs the addresses it actually bound, which is how the e2e suite
  discovers a `:0` port.

### Fixed

- **Non-injective series key (data corruption).** `SeriesKey` encoded a metric as
  `name{k=v,...}` with no escaping, so `{a: "b,c=d"}` and `{a: "b", c: "d"}` — and
  a name like `cpu{a=b}` against `cpu` with `{a: "b"}` — produced the *same* key.
  Two distinct series merged silently: points from one were returned by queries
  for the other, and the stored label set was whichever writer arrived first,
  across the memory, bolt and tsdb backends. Found by a fuzzed injectivity
  invariant. Fixed with a backslash-escaped, invertible encoding (`ParseSeriesKey`
  is the proof it cannot collide), a no-op for the clean keys real agents produce.
  Benchmark-driven optimization of the new encoder made it *faster* than the
  broken one it replaced: 4→1 allocations, −19% time at three labels.
- **Alert-fingerprint collision (alerts silently suppressed).** The same defect
  class in `alert.Fingerprint`: the label separator was neither escaped nor
  length-framed, so two different alerts could share a fingerprint and one would
  suppress the other in the dedup and grouping paths. Fixed by length-prefixing
  every hashed field.
- **Unbounded allocation in WAL replay (crash-loop DoS).** `Replay` read a
  `uint32` record length off the wire and did `make([]byte, length)`, so a torn or
  hostile header of `0xFFFFFFFF` allocated 4 GiB on startup. Now bounded by
  `maxRecordSize`, treating an oversized length like any other corrupt record.
- **int64 overflow in the chunk bounds check.** A corrupt `index.json` with a
  length near `math.MaxInt64` overflowed the `offset+length` guard, wrapped
  negative, passed the check and panicked with an out-of-range slice. Rewritten to
  compare each term against the data length separately.
- **NaN/Inf accepted over gRPC, rejected over HTTP.** `encoding/json` cannot carry
  a non-finite float, so the HTTP path rejected it at decode; protobuf carries it
  fine and `Metric.Validate` did not check, so a gRPC client could store a value
  an HTTP client could not. Closed at the shared `Validate` gate.
- **Unbounded per-agent map in the rate limiter (memory-exhaustion DoS).** One
  token bucket per key with no eviction, and the key is an attacker-chosen agent
  id or client IP: a stream of distinct keys grew the map until the process was
  OOM-killed, every request inside its own limit. Fixed with an idle sweep plus a
  hard cap and amortised batch eviction.
- **Storage write failures vanished from the pipeline counters.** A failed
  `WriteBatch` dropped its metrics silently; `ingested` would exceed
  `stored + invalid` with no counter to say by how much. Added a `failed` counter
  (and a dashboard tile), restoring the conservation identity
  `ingested == stored + invalid + failed`.

### Security

- pprof is no longer served on the API listener. `/debug/pprof/cmdline` exposes
  the process's argv (which can hold `-jwt-hs256-secret`) and `/debug/pprof/heap`
  exposes stored data; both now require an operator to bind `-pprof-addr`, and a
  non-loopback bind is warned about.
- The rate-limiter and series-key/fingerprint fixes above are each a
  denial-of-service or data-integrity issue closed in shipped code.

## [0.8.0] - 2026-07-09

`metricsctl`: a command-line client. Until now every interaction went through
curl or the browser, which is fine for a demo and useless for operating a
system. A CLI is a UI — if it is good, people use the product; if it is bad,
they wrap it in scripts and suffer. This one borrows kubectl's shape on purpose.

### Added

- `cmd/metricsctl` + `internal/cli`: a Cobra command tree — `query`, `stats`,
  `rules` (list/get/apply/preview/delete), `alerts list [--watch]`, `silences`
  (list/create/delete), `agents list`, `config`, `completion`, `version`.
- **Named contexts** (`internal/cli/config`), kubectl-style: one binary
  addresses production and staging without an edit in between. The file lives at
  `$METRICSCTL_CONFIG`, `$XDG_CONFIG_HOME/metricsctl/config.yaml` or
  `~/.metricsctl/config.yaml`, supports `${VAR}` expansion and `token-file`
  indirection, resolves `~` and config-relative paths, and is written `0600`.
  `metricsctl config view` redacts credentials unless `--show-secrets`.
- **Output formats** (`internal/cli/output`): `table` (hand-aligned, upper-cased
  headers, colour-aware column widths), `json` and `yaml` (which encode the raw
  API object, never the lossy table projection — a list stays a list), and `name`
  (one identifier per line, for `xargs`). Chosen with `-o`.
- **Declarative `rules apply -f`**: multi-document YAML, `-f -` for stdin,
  `--dry-run`, and a diff-style `created`/`updated`/`unchanged` report.
  `metadata.name` is the rule's stable id, so re-applying an unchanged file
  writes nothing — safe to run from CI on every push. The manifest is the desired
  state in full: a field it omits is reconciled back to the server's default,
  including a `for` clause the expression itself carries. Rules are compiled
  client-side before any request, so `--dry-run` catches a bad expression on the
  update path too, where the server is never asked.
- **`rules preview`**: backtest an expression over historical data before saving.
- **`alerts list --watch`**: redraw on an interval, clean exit on Ctrl+C.
- **Dynamic shell completion** for bash, zsh, fish and powershell:
  `metricsctl rules get <TAB>` asks the server which rules exist.
- **POSIX exit codes** as an API: `0` success, `1` generic, `2` usage, `3` auth,
  `4` not found — so `metricsctl rules get foo || handle_missing` works. Cobra's
  own argument and flag validation is wrapped so it exits `2`, an unknown or
  mistyped (sub)command is a usage error rather than a help screen and a silent
  `0`, and `rules apply` joins its per-rule failures so a `401` still exits `3`.
- **`agents list`**, derived from a heartbeat metric (`uptime_seconds` by
  default), because the server keeps no agent registry; documented as such.
- `make build` now also builds `bin/metricsctl`, with the version injected via
  `-ldflags -X main.version=$(git describe)`; `make install-ctl` installs it.
- `examples/alerting/rules.yaml` — a manifest for `rules apply`.

### Security

- The config is written through a fresh temporary file and renamed into place, so
  it is always `0600` and never half-written. (`os.WriteFile` applies its mode
  only when it *creates* the file: rewriting an already world-readable config
  would have left it world-readable with a new secret inside.) A loose-permissions
  file is warned about on every invocation.
- A context may configure exactly one credential, and `--api-key`/`--token`
  *replace* the context's rather than adding to it — otherwise the context's API
  key would ride along with the flag's bearer token to whatever `--server` now
  points at.
- Only the `${VAR}` placeholder form is expanded in the config. `os.ExpandEnv`
  would also eat the bare `$VAR` form, silently truncating any credential that
  contains a dollar sign.
- Colour and interactive prompts require a real terminal, detected with the
  terminal ioctl rather than the file mode — `/dev/null` is a character device
  too, and treating it as a terminal would write escape codes into redirected
  output and prompt where nobody can answer.
- Destructive commands (`rules delete`, `silences delete`) refuse to run
  unattended without `--yes`, instead of either hanging on a prompt or silently
  proceeding.
- `NO_COLOR` (see <https://no-color.org>) and `--no-color` always disable colour.

### Dependencies

- `github.com/spf13/cobra` — the point of this stage. Kept minimal otherwise:
  Viper, go-pretty and survey are replaced by the standard library (a hand-written
  table aligner and config loader, plain prompts), and `golang.org/x/term`
  supplies the one thing the stdlib cannot, terminal detection.
  `gopkg.in/yaml.v3` parses the config and rule manifests.

## [0.7.0] - 2026-07-09

Alerting: the system stops being purely passive (store and show) and becomes
active — it watches its own data, decides when something is wrong, and tells
someone. Rule evaluation and notification delivery are separated by a channel,
because one is periodic and deterministic while the other talks to services that
time out, rate-limit and fall over.

### Added

- `internal/alerting/rules`: a **PromQL-lite rule DSL** with a hand-written lexer
  and a recursive-descent parser (one function per grammar production), an AST
  and its evaluator. Comparisons *filter* (`cpu > 90` yields the breaching
  samples); range functions (`rate`, `increase`, `delta`,
  `{avg,min,max,sum,count,last,stddev}_over_time`) with counter-reset handling;
  instant functions (`abs`, `ceil`, `floor`, `round`, `clamp_min`, `clamp_max`);
  aggregations (`sum|avg|min|max|count|stddev`) with `by`/`without`; `and`, `or`,
  `unless`; label matchers `=`, `!=`, `=~`, `!~` with fully anchored regexes.
  Parse errors carry a byte position; input length, regex length and recursion
  depth are bounded.
- The **alert state machine** (`inactive → pending → firing → resolved`) with
  `for` semantics: an alert fires only after the condition has held
  *continuously*. Resolutions are always announced — including when the rule that
  produced them is deleted or disabled, and never for an alert that was silenced
  or inhibited and therefore never announced in the first place. Alert identity is
  a stable fingerprint over the rule ID plus sorted labels, so re-evaluation
  dedups instead of re-paging. Annotations are `text/template` expanded over the
  alert's value and labels.
- A **scheduler** (`rules.Manager`): one goroutine per rule ticking on its own
  interval, with a randomised start delay (so rules loaded together do not
  stampede storage), a per-iteration timeout, and hot reload without a restart.
- `internal/alerting/alert`: **grouping and dedup** — one notification for fifty
  failed hosts. `group_wait` / `group_interval` / `repeat_interval` scheduling,
  with a content hash so an unchanged group is not re-sent.
- `internal/alerting/silence`: **silences** (mute matching alerts for a window)
  with `=`, `!=`, `=~`, `!~` matchers; `internal/alerting/inhibit`: **inhibition
  rules** (a firing `HostDown` suppresses `CPUHigh` on the same host).
- `internal/alerting/notify`: the dispatcher, a **retry queue** with exponential
  backoff **and jitter** (a shared webhook must not be hit by a thundering herd
  of synchronised retries), and a **circuit breaker** per receiver — lock-free on
  the hot path, admitting exactly one probe while half-open.
- `internal/alerting/notify/receivers`: `log`, `webhook` (HMAC-signed with a
  signed timestamp against replay), `slack` (incoming webhook), and `email`
  (`net/smtp`, header-injection safe). Permanent failures (4xx other than
  408/429) are never retried.
- `internal/clock`: an injectable `Clock` (`Real` + a deterministic `Fake` with
  `Advance`/`BlockUntil`), so the time-heavy alerting logic is tested without
  sleeps.
- Tenant-scoped alerting API: `GET|POST /api/v1/rules`,
  `GET|PUT|DELETE /api/v1/rules/{id}`, `POST /api/v1/rules/preview` (backtest an
  expression over historical data without saving it), `GET /api/v1/alerts`,
  `GET|POST /api/v1/silences`, `DELETE /api/v1/silences/{id}`.
- Flags: `-alerting`, `-alert-rules`, `-alert-config`, `-alert-lookback`,
  `-alert-buffer` (plus the matching env vars). Off by default.
- `examples/alerting/{rules,receivers}.json` — a runnable sample configuration.

### Changed

- The live dashboard gained an **alerts panel**; the hub pushes tenant-scoped
  `alert` events over the existing WebSocket.
- RBAC extends to alerting: reading rules/alerts/silences needs the `query`
  action, mutating them needs `admin`.
- Multi-tenancy extends to alerting: a rule evaluates through a querier that
  force-injects `tenant=<owner>` into every storage query, and a tenant can
  neither see nor modify another tenant's rules, alerts or silences. A rule's
  tenant comes from the authenticated principal, never from the request body.
- `cmd/server`: the alerting service runs alongside the HTTP and gRPC servers
  and stops with them, before the pipeline drains and storage is closed.

### Security

- Webhook payloads are signed `sha256=HMAC(secret, "<unix-ts>.<body>")`; signing
  the timestamp is what stops a captured request from being replayed later.
- `tenant` is a reserved rule label. It is rejected at rule creation and
  re-stamped from the rule's owner during evaluation, so a rule can neither forge
  the tenant attribution of its alerts nor lose it to an aggregation such as
  `max by (agent_id) (…)`.
- Silences are applied at delivery time as well as on ingest, so a silence
  created after an alert was grouped still suppresses its repeat reminders.
- Email headers are sanitised, so a newline smuggled through a label cannot
  inject extra headers. The SMTP conversation runs on a connection with a dial
  timeout and an absolute deadline, and is closed on context cancellation —
  `smtp.SendMail` sets neither, so a peer that accepts and then goes silent would
  otherwise strand one goroutine per alert.
- A silence with no matchers is rejected: it would mute every alert in the system.

## [0.6.0] - 2026-07-09

An embedded live dashboard: a single-page app served from the binary and a
from-scratch WebSocket that streams live metrics and stats to the browser.

### Added

- `internal/server/live`: a WebSocket server implemented from scratch on the
  standard library (RFC 6455 handshake, frame codec, client-frame unmasking,
  fragmentation, ping/pong/close) — no third-party WebSocket library.
- A broadcast `Hub` (single-goroutine-owns-the-map pattern) that fans newly
  stored metrics and periodic stats snapshots out to connected dashboards, with
  non-blocking delivery (slow clients drop frames, never block the pipeline).
- Embedded SPA under `web/` (`go:embed`), served at `/` with assets at
  `/static/`; a live metric feed plus pipeline/storage counters and a chart.
- Pipeline observer hook (`SetObserver`) invoked with each stored batch.
- `-ui` / `UI` flag (default `true`) to serve the dashboard.

### Changed

- Multi-tenancy extends to the live stream: the `/ws` endpoint authenticates
  from query params (browsers can't set WS handshake headers) and scopes the
  feed to the caller's tenant; the stats stream is admin-only.
- `statusRecorder` (logging middleware) now forwards `Hijack` so the WebSocket
  upgrade works through the middleware chain.

## [0.5.0] - 2026-07-09

Authentication, role-based access control and multi-tenant data isolation across
both transports. Auth is off by default (backward compatible).

### Added

- `internal/auth`: authenticated `Principal` (subject, tenant, roles) carried
  through `context.Context`, plus an `Authenticator` chain.
- API-key authentication: `{subject, tenant, roles}` mapping loaded from a JSON
  file (`-api-keys`); keys stored only as SHA-256 hashes and resolved by hash.
- JWT bearer authentication implemented from scratch on the standard library
  (no third-party JWT lib): HS256 (`-jwt-hs256-secret`) and RS256 via a rotating
  JWKS key set (`-jwks-url`). The verifier is pinned to one algorithm (rejecting
  `none` and RS256→HS256 confusion); `exp` is mandatory; `nbf`/`iss`/`aud`
  validated; JWKS rejects sub-2048-bit RSA keys and refreshes on rotation.
- RBAC: roles `writer`/`reader`/`admin` grant actions ingest/query/admin, checked
  by an HTTP middleware and gRPC interceptors (unary + stream).
- Multi-tenancy: a server-assigned `tenant` label (never client-settable) is
  stamped on ingest and forced as a filter on every query, isolating series per
  tenant.
- Server flags `-auth`, `-api-keys`, `-jwt-hs256-secret`, `-jwks-url`,
  `-jwt-issuer`, `-jwt-audience`; agent flags `-api-key`, `-auth-token`.
- Tests: JWT (HS256/RS256, expiry, alg-confusion, issuer/audience), JWKS fetch +
  rotation + weak-key rejection, API keys, RBAC, and tenant-isolation E2E over
  both HTTP and gRPC.

### Changed

- The agent ships credentials via a `Credentials` value on both senders (HTTP
  headers / gRPC metadata).
- `model.Batch` gained a server-only `Tenant` field (`json:"-"`).

### Dependencies

- No new modules (auth is built on the standard library and existing gRPC deps).

## [0.4.0] - 2026-07-05

A gRPC + Protocol Buffers transport between agent and server, alongside HTTP.

### Added

- Protobuf-defined `metrics.v1.MetricsService` (`proto/metrics/v1/metrics.proto`)
  with all three RPC styles: `Ingest` (unary), `IngestStream` (bidirectional
  streaming) and `Query` (server streaming). Generated code lives in
  `internal/proto/metricspb` (regenerated with `make proto` or `go generate`).
- gRPC server (`internal/server/grpcserver`) that funnels batches into the same
  pipeline and store as HTTP, with panic-recovery and request-logging
  interceptors, server reflection, and a graceful-stop lifecycle mirroring the
  HTTP server.
- Agent gRPC transport: a single long-lived bidirectional stream reused across
  ticks (lockstep send/ack, reopened transparently on error), selectable with
  `-transport=grpc` / `-grpc-server`.
- `-grpc-addr` / `GRPC_ADDR` server flag (default `:9090`, empty to disable).
- `internal/grpcconv` for model <-> protobuf conversion.
- Tests: conversion round-trip, gRPC service integration over a loopback
  listener, and the agent streaming sender.

### Changed

- The agent now ships through a `Transport` interface; the HTTP sender is one
  implementation and the new gRPC streaming sender another.
- gRPC backpressure: unary `Ingest` returns `ResourceExhausted`; the stream
  replies with `IngestAck.throttled = true`.

### Dependencies

- Added `google.golang.org/grpc` and `google.golang.org/protobuf`.

## [0.3.0] - 2026-07-05

Persistent storage: two on-disk backends behind the `Storage` interface,
selectable at runtime.

### Added

- `bolt` backend (bbolt B+tree): metrics persisted with big-endian timestamp
  keys so a time-range query is a cursor range scan; writes batched into single
  transactions.
- `tsdb` backend: a from-scratch LSM-style engine — a CRC-checked write-ahead
  log (fsync + crash recovery), an in-memory head, and immutable chunks written
  atomically (fsync + rename) and read back via mmap with binary search; time
  pruning of chunks and a `flock` single-writer lock (lock-file fallback off Unix).
- `-storage` / `STORAGE` (`memory` | `bolt` | `tsdb`) and `-data-dir` / `DATA_DIR`
  to choose and locate the backend.
- Fuzz target for the chunk header parser.

### Changed

- `Storage` interface: `Write` now returns an `error`; added `WriteBatch` and
  `Close`. The pipeline's store stage batches metrics (by size or a 100ms timer)
  into `WriteBatch` calls.
- Exported shared query helpers (`SeriesKey`, `MatchLabels`, `FilterTime`,
  `ApplyQuery`) so every backend aggregates identically.

### Fixed

- Restore strict JSON decoding on ingest (reject unknown fields and trailing
  data), lost during the 0.2.0 pipeline rewrite.

### Dependencies

- Added `go.etcd.io/bbolt` (bolt backend) and `golang.org/x/sys` (mmap + flock).

## [0.2.0] - 2026-07-04

Rebuilt the server around a channel pipeline with an in-memory time-series
database and a query API.

### Added

- Channel-based ingestion pipeline (`ingest → unpack → validate → enrich → store`)
  with configurable per-stage worker pools and a bounded ingest buffer that
  drains cleanly on shutdown without losing in-flight metrics.
- In-memory time-series store with a metric-name index and canonical,
  label-sorted series keys that deduplicate series regardless of label order.
- Query API: `GET /api/v1/query` with a required metric `name`, arbitrary
  label-equality filters, and `from`/`to` time-window selection.
- Time-window aggregations: `avg`, `min`, `max`, `sum`, `count` and percentiles
  (`p50`/`p90`/`p95`/`p99`), bucketed by a configurable `step`, plus a result `limit`.
- Automatic `agent_id` label injection: the unpack stage tags every metric with
  its batch's agent id, so all series are queryable by `agent_id`.
- Per-agent token-bucket rate limiting keyed by the `X-Agent-ID` header (falling
  back to client IP), with configurable requests-per-second and burst.
- HTTP middleware chain: panic recovery, request-ID propagation (`X-Request-ID`),
  structured request logging, and rate limiting.
- Self-metrics at `GET /debug/stats` exposing pipeline counters
  (ingested/dropped/invalid/stored) and storage counters (series/points).
- Runtime profiling via `net/http/pprof` under `/debug/pprof/`.
- Configuration for ingest buffer, per-stage worker counts, and rate-limit
  RPS/burst (flags + environment variables).
- Unit tests for the new pipeline, storage, ratelimit and middleware packages;
  the server handler tests were rewritten for the pipeline.

### Changed

- Ingestion is now asynchronous: `POST /api/v1/metrics` returns `202 Accepted`
  on enqueue and `503 Service Unavailable` with a `Retry-After` header when the
  pipeline buffer is saturated (backpressure).
- The old `GET /api/v1/metrics` list endpoint is replaced by `GET /api/v1/query`.
- Error and query responses are now JSON (`{"error":"..."}`) instead of plaintext.
- The agent now sends an `X-Agent-ID` header on every batch it ships.

### Removed

- The old slice-backed in-memory storage (`internal/server/storage.go`: the
  `Storage` type with `Add`/`All`/`Count`).

### Dependencies

- Added `golang.org/x/time` v0.15.0 (`golang.org/x/time/rate`) for rate limiting.

## [0.1.0] - 2026-05-25

Initial MVP: an agent that collects host telemetry and ships it to an in-memory
collector server over HTTP.

### Added

- Agent: collects CPU usage, memory (total/used/percent), disk
  (total/used/percent for a configurable path) and uptime via `gopsutil`, on a
  configurable interval.
- Agent: runs collectors concurrently each tick with per-collector timeout and
  error isolation, then batches and POSTs the metrics as JSON.
- Reusable HTTP client (`pkg/httpx`) with timeout, connection pooling and
  automatic retries with exponential backoff on network errors, HTTP 429 and 5xx,
  safely replaying the request body between attempts.
- Server: in-memory collector with `POST /api/v1/metrics` (ingest) and
  `GET /api/v1/metrics` (list all), guarded by an `RWMutex` returning defensive
  copies.
- Hardened ingest: 1 MiB body limit, strict JSON decoding (rejects unknown
  fields and trailing data) and batch validation (`400` on bad input, `202` on
  success).
- `GET /healthz` liveness endpoint.
- Metric model: `Metric`/`Batch` with gauge/counter kinds, JSON (un)marshalling
  that accepts string or numeric type forms, and validation.
- Graceful shutdown on SIGINT/SIGTERM (the server drains in-flight requests with
  a 10s timeout).
- Configuration via flags with environment-variable fallbacks; structured JSON
  logging via `slog`.
- Unit tests across the agent, httpx, model and server packages; Makefile with
  build/test/vet/lint targets.

### Dependencies

- Go 1.26; `github.com/shirou/gopsutil/v4` for cross-platform metric collection.

[Unreleased]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ANTON-IVANOVICH/TraceForge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ANTON-IVANOVICH/TraceForge/releases/tag/v0.1.0
