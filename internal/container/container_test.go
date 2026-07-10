package container

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// mapFile wraps file content for fstest.MapFS.
func mapFile(data string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(data)} }

func TestMemoryLimit(t *testing.T) {
	// The v2 nested cases share this leaf path so the walk from leaf to root is
	// exercised across a real multi-segment path.
	const v2Path = "0::/kubepods/burstable/pod123/abc"

	tests := []struct {
		name    string
		files   fstest.MapFS
		proc    string
		procNil bool
		want    int64
		wantOK  bool
	}{
		{
			name:    "v2 root only",
			files:   fstest.MapFS{"memory.max": mapFile("268435456")},
			procNil: true,
			want:    268435456,
			wantOK:  true,
		},
		{
			name:    "v2 max means no limit",
			files:   fstest.MapFS{"memory.max": mapFile("max")},
			procNil: true,
			wantOK:  false,
		},
		{
			// The leaf declares "max", so the answer can only be right if the walk
			// climbs to the parent and compares it against the root, taking the
			// minimum. 536870912 is neither the leaf's nor the root's own value.
			name: "v2 nested parent is tightest",
			files: fstest.MapFS{
				"kubepods/burstable/pod123/abc/memory.max": mapFile("max"),
				"kubepods/burstable/pod123/memory.max":     mapFile("536870912"),
				"memory.max":                               mapFile("1073741824"),
			},
			proc:   v2Path,
			want:   536870912,
			wantOK: true,
		},
		{
			// Same chain, but now the leaf is the smallest link, so the leaf must
			// win over the looser parent and root.
			name: "v2 nested leaf is tightest",
			files: fstest.MapFS{
				"kubepods/burstable/pod123/abc/memory.max": mapFile("104857600"),
				"kubepods/burstable/pod123/memory.max":     mapFile("536870912"),
				"memory.max":                               mapFile("1073741824"),
			},
			proc:   v2Path,
			want:   104857600,
			wantOK: true,
		},
		{
			name:    "v2 whitespace is trimmed",
			files:   fstest.MapFS{"memory.max": mapFile("  268435456\n")},
			procNil: true,
			want:    268435456,
			wantOK:  true,
		},
		{
			name:    "v1 sentinel means no limit",
			files:   fstest.MapFS{"memory/memory.limit_in_bytes": mapFile("9223372036854771712")},
			procNil: true,
			wantOK:  false,
		},
		{
			name:    "v1 real number",
			files:   fstest.MapFS{"memory/memory.limit_in_bytes": mapFile("2147483648")},
			procNil: true,
			want:    2147483648,
			wantOK:  true,
		},
		{
			// The controller root is looser than the process's own leaf, so the
			// answer is right only if the leaf under memory/<path>/ is read too.
			name: "v1 own leaf is tighter than root",
			files: fstest.MapFS{
				"memory/memory.limit_in_bytes":               mapFile("2147483648"),
				"memory/kubepods/pod9/memory.limit_in_bytes": mapFile("536870912"),
			},
			proc:   "5:memory:/kubepods/pod9",
			want:   536870912,
			wantOK: true,
		},
		{
			// A real v1 host co-mounts controllers, so the memory controller shares
			// a line with cpu and cpuacct. The leaf limit is only found if the
			// comma-separated controller list is split and "memory" matched inside
			// it. The cpuset line names a path that must not be read as the memory
			// leaf, and the systemd-style "0::" v2 line in a v1 file must not match
			// as a memory controller either; if either were mistaken for memory the
			// process's own tighter leaf would be lost and the loose root returned.
			name: "v1 co-mounted controllers leaf is tighter",
			files: fstest.MapFS{
				"memory/memory.limit_in_bytes":                         mapFile("2147483648"),
				"memory/kubepods/burstable/pod9/memory.limit_in_bytes": mapFile("536870912"),
			},
			proc:   "2:cpuset:/foo\n3:cpu,cpuacct,memory:/kubepods/burstable/pod9\n0::/kubepods/burstable/pod9",
			want:   536870912,
			wantOK: true,
		},
		{
			name:    "garbage content",
			files:   fstest.MapFS{"memory.max": mapFile("not-a-number")},
			procNil: true,
			wantOK:  false,
		},
		{
			name:    "empty file",
			files:   fstest.MapFS{"memory.max": mapFile("")},
			procNil: true,
			wantOK:  false,
		},
		{
			name:    "negative value",
			files:   fstest.MapFS{"memory.max": mapFile("-1")},
			procNil: true,
			wantOK:  false,
		},
		{
			name:    "zero value",
			files:   fstest.MapFS{"memory.max": mapFile("0")},
			procNil: true,
			wantOK:  false,
		},
		{
			name:    "no files at all",
			files:   fstest.MapFS{},
			procNil: true,
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r io.Reader
			if !tc.procNil {
				r = strings.NewReader(tc.proc)
			}
			got, ok := memoryLimit(tc.files, r)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("memoryLimit() = (%d, %t), want (%d, %t)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// swapSetMemoryLimit replaces the debug.SetMemoryLimit seam for the duration of
// the test, so a test can watch the value handed to the runtime without changing
// the test process's own heap limit.
func swapSetMemoryLimit(t *testing.T, f func(int64) int64) {
	t.Helper()
	old := setMemoryLimit
	setMemoryLimit = f
	t.Cleanup(func() { setMemoryLimit = old })
}

// swapReadMemoryLimit replaces the cgroup-reading seam, so ApplyMemoryLimit can
// be driven on a machine that has no cgroup (a developer's Mac) as if it had a
// known one.
func swapReadMemoryLimit(t *testing.T, f func() (int64, bool)) {
	t.Helper()
	old := readMemoryLimit
	readMemoryLimit = f
	t.Cleanup(func() { readMemoryLimit = old })
}

// unsetGOMEMLIMIT guarantees GOMEMLIMIT is absent for the test, restoring any
// prior value afterward. The tests that exercise the ratio path must not be
// silently short-circuited by an operator value inherited from the environment.
func unsetGOMEMLIMIT(t *testing.T) {
	t.Helper()
	if v, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		_ = os.Unsetenv("GOMEMLIMIT")
		t.Cleanup(func() { _ = os.Setenv("GOMEMLIMIT", v) })
	}
}

func TestApplyMemoryLimitHonoursEnv(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "123456789")

	// A real limit is injected so that, if the honour check were removed,
	// ApplyMemoryLimit would proceed and call setMemoryLimit — which this test
	// would then catch. Without the injection the call would be skipped anyway on
	// a non-Linux machine and the regression would hide.
	swapReadMemoryLimit(t, func() (int64, bool) { return 1 << 30, true })

	called := false
	swapSetMemoryLimit(t, func(int64) int64 { called = true; return 0 })

	got, ok := ApplyMemoryLimit(0.9, nil)
	if got != 0 || ok {
		t.Fatalf("ApplyMemoryLimit() = (%d, %t), want (0, false)", got, ok)
	}
	if called {
		t.Fatal("SetMemoryLimit was called despite an operator-set GOMEMLIMIT")
	}
}

func TestApplyMemoryLimitRatioFallback(t *testing.T) {
	unsetGOMEMLIMIT(t)

	limit := int64(1) << 30 // 1 GiB; a var, not a const, so float64(limit)*ratio is not constant-folded
	swapReadMemoryLimit(t, func() (int64, bool) { return limit, true })

	for _, ratio := range []float64{0, 1.5, -1, 100} {
		t.Run(fmt.Sprintf("ratio=%v", ratio), func(t *testing.T) {
			var applied int64
			swapSetMemoryLimit(t, func(n int64) int64 { applied = n; return 0 })

			got, ok := ApplyMemoryLimit(ratio, nil)
			want := int64(float64(limit) * defaultRatio)
			if !ok || got != want {
				t.Fatalf("ApplyMemoryLimit(%v) = (%d, %t), want (%d, true)", ratio, got, ok, want)
			}
			if applied != want {
				t.Fatalf("SetMemoryLimit got %d, want %d (fallback ratio %v not applied)", applied, want, defaultRatio)
			}
		})
	}
}

func TestApplyMemoryLimitAppliesRatio(t *testing.T) {
	unsetGOMEMLIMIT(t)

	limit := int64(800) << 20
	swapReadMemoryLimit(t, func() (int64, bool) { return limit, true })

	var applied int64
	swapSetMemoryLimit(t, func(n int64) int64 { applied = n; return 0 })

	got, ok := ApplyMemoryLimit(0.5, nil)
	want := int64(float64(limit) * 0.5)
	if !ok || got != want || applied != want {
		t.Fatalf("ApplyMemoryLimit(0.5) = (%d, %t), applied %d; want got==applied==%d, ok==true", got, ok, applied, want)
	}
}

func TestApplyMemoryLimitNoCgroupLimit(t *testing.T) {
	unsetGOMEMLIMIT(t)
	swapReadMemoryLimit(t, func() (int64, bool) { return 0, false })

	called := false
	swapSetMemoryLimit(t, func(int64) int64 { called = true; return 0 })

	got, ok := ApplyMemoryLimit(0.9, nil)
	if got != 0 || ok {
		t.Fatalf("ApplyMemoryLimit() = (%d, %t), want (0, false)", got, ok)
	}
	if called {
		t.Fatal("SetMemoryLimit was called when no cgroup limit was found")
	}
}

// TestMemoryLimitNonLinux pins the public contract on platforms without cgroups:
// MemoryLimit reports no limit and, just as importantly, does not panic reaching
// for /proc or /sys that are not there. On Linux the real cgroup governs, and
// that path is covered by TestMemoryLimit against a synthetic filesystem.
func TestMemoryLimitNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("MemoryLimit consults the real cgroup on Linux")
	}
	if got, ok := MemoryLimit(); got != 0 || ok {
		t.Fatalf("MemoryLimit() on %s = (%d, %t), want (0, false)", runtime.GOOS, got, ok)
	}
}

// TestGOMAXPROCS asserts only what is observable: the helper returns a positive
// value that agrees with runtime.GOMAXPROCS(0). The stronger property — that the
// helper never sets sched.customGOMAXPROCS and so never disables the runtime's
// cgroup-quota re-derivation — is structural, guaranteed by reading through
// runtime/metrics (which has no setter), and cannot be checked here because Go
// exposes no way to observe that flag.
func TestGOMAXPROCS(t *testing.T) {
	got := GOMAXPROCS()
	if got <= 0 {
		t.Fatalf("GOMAXPROCS() = %d, want a positive value", got)
	}
	if want := runtime.GOMAXPROCS(0); got != want {
		t.Fatalf("GOMAXPROCS() = %d, want %d", got, want)
	}
}
