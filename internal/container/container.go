// Package container teaches a Go process what its cgroup actually allows, so a
// containerised deployment neither throws away the runtime's automatic tuning
// nor walks into an OOM kill.
//
// GOMAXPROCS is deliberately left untouched. Since Go 1.25 the runtime derives
// the default on Linux from the cgroup CPU quota (cpu.max in v2,
// cpu.cfs_quota_us/cpu.cfs_period_us in v1), and it re-derives it up to once a
// second as the quota changes. The derivation is not a plain minimum, and the
// difference matters in exactly the container this package exists for
// (runtime/cgroup_linux.go, adjustCgroupGOMAXPROCS):
//
//	GOMAXPROCS = min(CPUs from sched_getaffinity, max(ceil(quota/period), 2))
//
// So a fractional quota rounds up — `--cpus=1.5` yields 2, not 1 — and a quota
// below two cores is floored at two, unless the affinity mask itself allows fewer.
// `--cpus=1` on an eight-core host gives GOMAXPROCS=2.
//
// Setting the GOMAXPROCS environment variable — which almost every Helm chart does
// through the downward API — or calling runtime.GOMAXPROCS with a positive value
// disables that automatic updating (see `go doc runtime.GOMAXPROCS`). Note that
// runtime.GOMAXPROCS(0) is a safe read: the runtime returns before it marks the
// value custom. The neighbouring runtime.GOMAXPROCS(runtime.NumCPU()), which looks
// like an equally harmless no-op, is not.
//
// This package therefore never sets GOMAXPROCS; it only reports what the runtime
// chose, so that a vertical-pod-autoscaler bump to the CPU limit is still picked
// up mid-run.
//
// GOMEMLIMIT is the opposite case: the runtime does not derive it from the
// cgroup at all. Left unset, a Go heap grows until it reaches the container's
// memory limit and the kernel OOM-kills the process, where a soft limit would
// have driven a GC instead. So this package reads the cgroup memory limit and
// calls debug.SetMemoryLimit.
//
// The ratio is a knob rather than a constant because GOMEMLIMIT is not the whole
// accounting. It bounds only the memory the Go runtime manages. It does not
// bound the binary's text, allocations made by cgo, or file pages brought in by
// mmap — and this project's TSDB mmaps its chunk files
// (internal/server/storage/tsdb/chunk), so those pages count against the cgroup
// limit while remaining invisible to GOMEMLIMIT. The headroom left by a ratio
// below one is where those uncounted pages live; how much to leave depends on
// the deployment, which is why the caller supplies it.
package container

import (
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
)

// cgroupRoot is the conventional mount point of the cgroup filesystem on Linux.
const cgroupRoot = "/sys/fs/cgroup"

// defaultRatio is the fraction of the cgroup memory limit handed to GOMEMLIMIT
// when the caller's ratio is nonsensical. It leaves a tenth of the container for
// the uncounted pages — text, cgo, mmap'd chunks — described in the package doc.
const defaultRatio = 0.9

// v1NoLimit is the sentinel cgroup v1 uses for "unlimited": a value close to the
// signed 64-bit maximum, page-aligned by the kernel. Any value at or above it is
// not a real limit, so we treat the whole upper range as unlimited rather than
// matching one exact constant that a future kernel might round differently.
const v1NoLimit = int64(1) << 62

// These are seams for tests. Production wires them to the real runtime call and
// the real cgroup reader; a test swaps them to exercise ApplyMemoryLimit on a
// machine that has no cgroup at all (a developer's Mac) and to observe the value
// handed to the runtime without perturbing the test process's own heap limit.
var (
	setMemoryLimit  = debug.SetMemoryLimit
	readMemoryLimit = MemoryLimit
)

// MemoryLimit reports the memory limit this process runs under, in bytes. The
// second result is false when there is no limit or none can be determined.
//
// The non-Linux short-circuit is a runtime.GOOS check rather than a build tag on
// purpose: a build tag would drop this file — and the whole cgroup parser with
// it — from every macOS and Windows build, so the parser would never compile on
// the developer's machine and would rot there unnoticed. Guarding at runtime
// keeps the parser compiled and tested on every platform while still doing
// nothing where /sys/fs/cgroup does not exist.
func MemoryLimit() (int64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}

	cgroupFS := os.DirFS(cgroupRoot)

	// /proc/self/cgroup names the leaf cgroup this process sits in; without it we
	// can still read the root limits, just not the tighter ones set on the leaf.
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return memoryLimit(cgroupFS, nil)
	}
	defer func() { _ = f.Close() }()

	return memoryLimit(cgroupFS, f)
}

// memoryLimit is the pure core of MemoryLimit. cgroupFS is rooted where
// /sys/fs/cgroup would be mounted, and procSelfCgroup carries the contents of
// /proc/self/cgroup and may be nil. It never fails and never panics: a parse
// error, a missing file, or a nonsensical value is reported as "no limit", since
// this runs at startup on every platform and must not take the process down.
func memoryLimit(cgroupFS fs.FS, procSelfCgroup io.Reader) (int64, bool) {
	var cgroup string
	if procSelfCgroup != nil {
		// A read error leaves cgroup empty, which degrades to reading the root
		// limits — the same fallback as a missing /proc/self/cgroup.
		if b, err := io.ReadAll(procSelfCgroup); err == nil {
			cgroup = string(b)
		}
	}

	// v2 is the modern unified hierarchy; try it first. Only if it yields nothing
	// do we fall back to a v1 layout, which lives under different paths and cannot
	// be mistaken for it.
	if limit, ok := memoryLimitV2(cgroupFS, cgroup); ok {
		return limit, true
	}
	return memoryLimitV1(cgroupFS, cgroup)
}

// memoryLimitV2 reads the effective cgroup v2 memory limit: the minimum of
// memory.max along the whole chain from the process's leaf cgroup up to the
// root. A parent's limit binds its children no matter what the leaf declares, so
// the tightest link in the chain is the one that governs. Levels whose
// memory.max is missing or set to "max" simply do not constrain and are skipped.
func memoryLimitV2(cgroupFS fs.FS, cgroup string) (int64, bool) {
	// fs.FS paths are always relative, so the leading slash of the cgroup path is
	// stripped and every level is addressed with path.Join.
	dir := strings.TrimPrefix(cgroupV2Path(cgroup), "/")

	var limit int64
	var found bool
	for {
		name := "memory.max"
		if dir != "" {
			name = path.Join(dir, "memory.max")
		}
		if v, ok := readLimit(cgroupFS, name, parseV2Max); ok && (!found || v < limit) {
			limit, found = v, true
		}

		if dir == "" {
			break
		}
		if parent := path.Dir(dir); parent == "." {
			dir = ""
		} else {
			dir = parent
		}
	}
	return limit, found
}

// memoryLimitV1 reads the cgroup v1 memory limit. Unlike v2 it does not walk the
// whole chain: memory.limit_in_bytes reflects the hierarchy poorly (a child can
// carry a looser number than its parent), so a full walk would buy little
// correctness for the complexity. Reading the controller root and, when known,
// the process's own leaf and taking the smaller of the two is enough in
// practice.
func memoryLimitV1(cgroupFS fs.FS, cgroup string) (int64, bool) {
	var limit int64
	var found bool
	consider := func(dir string) {
		name := path.Join("memory", dir, "memory.limit_in_bytes")
		if v, ok := readLimit(cgroupFS, name, parseV1Limit); ok && (!found || v < limit) {
			limit, found = v, true
		}
	}

	consider("")
	if p, ok := cgroupV1MemoryPath(cgroup); ok {
		consider(strings.TrimPrefix(p, "/"))
	}
	return limit, found
}

// cgroupV2Path returns the cgroup path from the single unified-hierarchy line of
// /proc/self/cgroup, which has the form "0::<path>". It returns "" when there is
// no such line, which addresses the root of the hierarchy.
func cgroupV2Path(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest
		}
	}
	return ""
}

// cgroupV1MemoryPath returns the path of the memory controller from
// /proc/self/cgroup, whose v1 lines have the form "<id>:<controllers>:<path>".
// A single line can list several comma-separated controllers, so each is
// checked.
func cgroupV1MemoryPath(cgroup string) (string, bool) {
	for _, line := range strings.Split(cgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, ctrl := range strings.Split(parts[1], ",") {
			if ctrl == "memory" {
				return parts[2], true
			}
		}
	}
	return "", false
}

// readLimit reads name from cgroupFS and parses it with parse. A missing file or
// a parse failure is reported as "no value" (false), never an error, so a caller
// walking a chain can skip the level and keep going.
func readLimit(cgroupFS fs.FS, name string, parse func(string) (int64, bool)) (int64, bool) {
	b, err := fs.ReadFile(cgroupFS, name)
	if err != nil {
		return 0, false
	}
	return parse(strings.TrimSpace(string(b)))
}

// parseV2Max interprets a cgroup v2 memory.max value. The literal "max" means
// the level sets no limit and is reported as no value; anything else must be a
// positive byte count.
func parseV2Max(s string) (int64, bool) {
	if s == "max" {
		return 0, false
	}
	return parsePositive(s)
}

// parseV1Limit interprets a cgroup v1 memory.limit_in_bytes value. v1 encodes
// "unlimited" as a huge number rather than a keyword, so any value in the top of
// the range is reported as no value.
func parseV1Limit(s string) (int64, bool) {
	v, ok := parsePositive(s)
	if !ok || v >= v1NoLimit {
		return 0, false
	}
	return v, true
}

// parsePositive parses a decimal byte count. Zero, negatives, and anything that
// is not a plain integer are rejected as no value: a limit of zero or below is
// not a limit a process could run under, and garbage must not become a limit.
func parsePositive(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// ApplyMemoryLimit sets GOMEMLIMIT to ratio times the cgroup memory limit. It
// returns the limit it applied, in bytes, and whether it applied one. A nil
// logger falls back to slog.Default.
func ApplyMemoryLimit(ratio float64, logger *slog.Logger) (int64, bool) {
	if logger == nil {
		logger = slog.Default()
	}

	// An operator who set GOMEMLIMIT explicitly outranks us: the runtime has
	// already applied their value, and recomputing it from the cgroup would swap
	// our guess for their decision behind their back.
	if v, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		logger.Info("GOMEMLIMIT set in the environment; honouring the operator's value", "GOMEMLIMIT", v)
		return 0, false
	}

	if ratio <= 0 || ratio > 1 {
		logger.Warn("memory limit ratio out of range; using the default", "ratio", ratio, "default", defaultRatio)
		ratio = defaultRatio
	}

	limit, ok := readMemoryLimit()
	if !ok {
		return 0, false
	}

	applied := int64(float64(limit) * ratio)
	setMemoryLimit(applied)
	logger.Info("GOMEMLIMIT derived from cgroup memory limit",
		"cgroup_limit_bytes", limit, "ratio", ratio, "gomemlimit_bytes", applied)
	return applied, true
}

// GOMAXPROCS reports the runtime's current GOMAXPROCS so callers can log it
// without importing runtime themselves.
//
// It reads the value through runtime/metrics rather than runtime.GOMAXPROCS
// because runtime/metrics has no setter: there is simply no argument this
// function could pass that would set sched.customGOMAXPROCS and thereby disable
// the runtime's once-per-second cgroup-quota re-derivation described in the
// package doc. The guarantee that this helper never disables that tracking is
// therefore structural — the dangerous call is unrepresentable here — not a
// property any test could observe, because Go exposes no way to read
// sched.customGOMAXPROCS back out.
//
// If the metric is ever removed or changes kind, we fall back to
// runtime.GOMAXPROCS(0), which the runtime source shows returns before the
// custom-flag assignment for a non-positive argument, so the fallback is as safe
// as the metric it replaces.
func GOMAXPROCS() int {
	sample := []metrics.Sample{{Name: "/sched/gomaxprocs:threads"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 {
		return int(sample[0].Value.Uint64())
	}
	return runtime.GOMAXPROCS(0)
}
