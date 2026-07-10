package buildinfo

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

// binfo builds a *debug.BuildInfo the way ReadBuildInfo would hand one over:
// mainVer becomes Main.Version, goVer becomes GoVersion, and settings is a flat
// key/value list turned into BuildSettings. It lets each resolve case state only
// the metadata it exercises.
func binfo(mainVer, goVer string, settings ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{GoVersion: goVer}
	bi.Main.Version = mainVer
	for i := 0; i+1 < len(settings); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return bi
}

func TestResolve(t *testing.T) {
	// The runtime fallbacks are whatever this test binary was built as; naming
	// them keeps the expectations honest instead of copying the same call the
	// code under test makes.
	runtimeGo := runtime.Version()
	runtimePlatform := runtime.GOOS + "/" + runtime.GOARCH

	tests := []struct {
		name                        string
		ldVersion, ldCommit, ldDate string
		bi                          *debug.BuildInfo
		ok                          bool
		want                        Info
	}{
		{
			name:      "ldflags only, no build info",
			ldVersion: "v1.2.3",
			ldCommit:  "deadbeefcafe",
			ldDate:    "2026-01-01T00:00:00Z",
			bi:        nil,
			ok:        false,
			want: Info{
				Version:   "v1.2.3",
				Commit:    "deadbeefcafe",
				Date:      "2026-01-01T00:00:00Z",
				Dirty:     false,
				GoVersion: runtimeGo,
				Platform:  runtimePlatform,
			},
		},
		{
			name: "build info only",
			bi: binfo("v0.9.0", "go1.26.2",
				"vcs.revision", "abcdef123456789",
				"vcs.time", "2026-07-10T09:00:00Z",
				"vcs.modified", "false",
				"GOOS", "linux",
				"GOARCH", "arm64"),
			ok: true,
			want: Info{
				Version:   "v0.9.0",
				Commit:    "abcdef123456",
				Date:      "2026-07-10T09:00:00Z",
				Dirty:     false,
				GoVersion: "go1.26.2",
				Platform:  "linux/arm64",
			},
		},
		{
			name:      "both sources, ldflags wins field by field",
			ldVersion: "v1.0.0",
			ldCommit:  "shortsha",
			ldDate:    "2026-02-02T00:00:00Z",
			bi: binfo("v0.9.0", "go1.26.2",
				"vcs.revision", "abcdef123456789",
				"vcs.time", "2026-07-10T09:00:00Z",
				"vcs.modified", "false",
				"GOOS", "linux",
				"GOARCH", "arm64"),
			ok: true,
			want: Info{
				Version:   "v1.0.0",
				Commit:    "shortsha",
				Date:      "2026-02-02T00:00:00Z",
				Dirty:     false,
				GoVersion: "go1.26.2",
				Platform:  "linux/arm64",
			},
		},
		{
			name: "nil build info, ok false, nothing injected",
			bi:   nil,
			ok:   false,
			want: Info{
				Version:   "dev",
				Commit:    "unknown",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: runtimeGo,
				Platform:  runtimePlatform,
			},
		},
		{
			name: "nil build info but ok true does not panic",
			bi:   nil,
			ok:   true,
			want: Info{
				Version:   "dev",
				Commit:    "unknown",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: runtimeGo,
				Platform:  runtimePlatform,
			},
		},
		{
			name: "(devel) main version falls back to dev",
			bi:   binfo("(devel)", "go1.26.2"),
			ok:   true,
			want: Info{
				Version:   "dev",
				Commit:    "unknown",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: "go1.26.2",
				Platform:  runtimePlatform,
			},
		},
		{
			name: "long revision truncated to twelve chars",
			bi: binfo("", "go1.26.2",
				"vcs.revision", "0123456789abcdef0123",
				"GOOS", "darwin",
				"GOARCH", "amd64"),
			ok: true,
			want: Info{
				Version:   "dev",
				Commit:    "0123456789ab",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: "go1.26.2",
				Platform:  "darwin/amd64",
			},
		},
		{
			name: "short revision left alone",
			bi: binfo("", "go1.26.2",
				"vcs.revision", "abc123",
				"GOOS", "linux",
				"GOARCH", "arm64"),
			ok: true,
			want: Info{
				Version:   "dev",
				Commit:    "abc123",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: "go1.26.2",
				Platform:  "linux/arm64",
			},
		},
		{
			// The release pipeline injects the full 40-character SHA via ldflags;
			// every other case here passes a short one, so this is the only case
			// that exercises truncateCommit on the ldflags path at all.
			name:     "full ldflags SHA truncated to twelve chars",
			ldCommit: "0123456789abcdef0123456789abcdef01234567",
			bi:       nil,
			ok:       false,
			want: Info{
				Version:   "dev",
				Commit:    "0123456789ab",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: runtimeGo,
				Platform:  runtimePlatform,
			},
		},
		{
			name:      "dirty tree with ldflags version still reports dirty",
			ldVersion: "v2.0.0",
			bi: binfo("v0.9.0", "go1.26.2",
				"vcs.revision", "feedface0000",
				"vcs.modified", "true",
				"GOOS", "linux",
				"GOARCH", "amd64"),
			ok: true,
			want: Info{
				Version:   "v2.0.0",
				Commit:    "feedface0000",
				Date:      "unknown",
				Dirty:     true,
				GoVersion: "go1.26.2",
				Platform:  "linux/amd64",
			},
		},
		{
			name:     "dirty tree with ldflags commit still reports dirty",
			ldCommit: "releasesha",
			bi: binfo("", "go1.26.2",
				"vcs.revision", "0000111122223333",
				"vcs.modified", "true",
				"GOOS", "linux",
				"GOARCH", "amd64"),
			ok: true,
			want: Info{
				Version:   "dev",
				Commit:    "releasesha",
				Date:      "unknown",
				Dirty:     true,
				GoVersion: "go1.26.2",
				Platform:  "linux/amd64",
			},
		},
		{
			name: "partial platform and empty go version fall back to runtime",
			bi: binfo("v0.5.0", "",
				"vcs.revision", "aaaabbbbcccc",
				"GOOS", "plan9"),
			ok: true,
			want: Info{
				Version:   "v0.5.0",
				Commit:    "aaaabbbbcccc",
				Date:      "unknown",
				Dirty:     false,
				GoVersion: runtimeGo,
				Platform:  runtimePlatform,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(tt.ldVersion, tt.ldCommit, tt.ldDate, tt.bi, tt.ok)
			if got != tt.want {
				t.Errorf("resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestResolveTruncatesToExactlyTwelve pins the truncation length independently
// of the table's literal, so a change from 12 to some other bound is caught even
// if someone updates the table to match.
func TestResolveTruncatesToExactlyTwelve(t *testing.T) {
	got := resolve("", "", "", binfo("", "go1.26.2", "vcs.revision", "0123456789abcdef0123"), true)
	if len(got.Commit) != 12 {
		t.Fatalf("commit length = %d, want 12 (%q)", len(got.Commit), got.Commit)
	}
}

// TestResolveTruncatesLdflagsCommitToCommitLen pins the ldflags-path truncation
// to the constant rather than the literal 12, so moving commitLen is caught here
// instead of silently accommodated. It uses the ldflags argument specifically
// because the VCS path is covered elsewhere and the release pipeline is the one
// that actually feeds a 40-character SHA.
func TestResolveTruncatesLdflagsCommitToCommitLen(t *testing.T) {
	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	got := resolve("", fullSHA, "", nil, false)
	if want := fullSHA[:commitLen]; got.Commit != want {
		t.Errorf("ldflags commit = %q, want %q", got.Commit, want)
	}
	if len(got.Commit) != commitLen {
		t.Errorf("ldflags commit length = %d, want commitLen=%d (%q)", len(got.Commit), commitLen, got.Commit)
	}
}

func TestGetIsMemoised(t *testing.T) {
	// Hammer Get() from several goroutines so a non-memoised implementation
	// (one that re-resolves per call) would drive resolveCount past 1. The
	// answer is 1 whether or not an earlier test in this package already called
	// Get(): the closure runs at most once for the whole process.
	const goroutines = 16
	var wg sync.WaitGroup
	got := make([]Info, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = Get()
		}()
	}
	wg.Wait()

	if resolveCount != 1 {
		t.Errorf("resolveCount = %d, want 1 (the closure must run exactly once)", resolveCount)
	}

	first := Get()
	for _, g := range got {
		if g != first {
			t.Errorf("Get() returned differing values across calls: %+v vs %+v", g, first)
		}
	}
	// A resolved build always has a platform; catching the zero value guards
	// against Get() handing back an empty Info that happens to equal itself.
	if first.Platform == "" {
		t.Errorf("Get() returned an unresolved Info: %+v", first)
	}
}

func TestString(t *testing.T) {
	clean := Info{
		Version:   "v0.11.0",
		Commit:    "a1b2c3d4e5f6",
		Date:      "2026-07-10T09:00:00Z",
		GoVersion: "go1.26.2",
		Platform:  "linux/arm64",
	}
	const wantClean = "v0.11.0 (a1b2c3d4e5f6, 2026-07-10T09:00:00Z, go1.26.2, linux/arm64)"
	if got := clean.String(); got != wantClean {
		t.Errorf("String() = %q, want %q", got, wantClean)
	}
	if !strings.Contains(clean.String(), clean.Version) {
		t.Errorf("String() %q does not contain version %q", clean.String(), clean.Version)
	}

	dirty := clean
	dirty.Dirty = true
	if got := dirty.String(); !strings.Contains(got, "[dirty]") {
		t.Errorf("dirty String() = %q, want it to contain %q", got, "[dirty]")
	}
	if got := clean.String(); strings.Contains(got, "[dirty]") {
		t.Errorf("clean String() = %q, must not contain %q", got, "[dirty]")
	}

	// The unknown-commit build must still yield a readable line rather than an
	// empty or malformed one.
	unknown := Info{Version: "dev", Commit: "unknown", Date: "unknown", GoVersion: "go1.26.2", Platform: "linux/arm64"}
	line := unknown.String()
	if !strings.Contains(line, "dev") || !strings.Contains(line, "unknown") {
		t.Errorf("unknown-commit String() = %q, want it to mention version and unknown", line)
	}
}

func TestShort(t *testing.T) {
	i := Info{Version: "v0.11.0", Commit: "a1b2c3d4e5f6"}
	if got := i.Short(); got != "v0.11.0" {
		t.Errorf("Short() = %q, want %q", got, "v0.11.0")
	}
}

func TestJSONKeys(t *testing.T) {
	i := Info{
		Version:   "v0.11.0",
		Commit:    "a1b2c3d4e5f6",
		Date:      "2026-07-10T09:00:00Z",
		Dirty:     true,
		GoVersion: "go1.26.2",
		Platform:  "linux/arm64",
	}
	blob, err := json.Marshal(i)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	for _, key := range []string{"version", "commit", "date", "dirty", "go_version", "platform"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("marshalled JSON %s is missing documented key %q", blob, key)
		}
	}

	// Round-tripping proves the tags are wired to the right fields, not merely
	// that some key of each name exists.
	var back Info
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal into Info: %v", err)
	}
	if back != i {
		t.Errorf("round-trip = %+v, want %+v", back, i)
	}
}
