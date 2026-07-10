// Package buildinfo is the single source of truth for what build is running.
//
// Two mechanisms report a build's identity and they disagree by design:
//
//   - -ldflags "-X metrics-system/internal/buildinfo.version=v0.11.0" injects
//     the values the release pipeline knows — the Makefile, the Docker build and
//     GoReleaser all set them. This is the only source that carries a real
//     semantic version, because a tag lives in the release process, not in the
//     source tree.
//
//   - runtime/debug.ReadBuildInfo() reads a table the Go toolchain bakes into the
//     binary. For `go build` and `go install` of a main package, inside a work
//     tree, with git on PATH, that table carries the exact commit and whether the
//     tree was dirty — which the pipeline can get wrong: a tag someone forgot to
//     move, or a release cut from an uncommitted change.
//
//     It is narrower than it sounds. `go test` never stamps a test binary, and
//     `go run` does not stamp either, so a test in this package cannot see the
//     real build's commit and `go run ./cmd/metricsctl version` reports
//     "dev (unknown, unknown, …)". Every field therefore has a placeholder rather
//     than an empty string, so no caller has to special-case an absence.
//
// ldflags wins field by field, because a deliberate release stamp is more
// trustworthy than an inferred one — except for the dirty bit, which is taken
// from VCS whenever VCS knows it. A release built from a modified tree is
// exactly the situation an operator debugging a bad rollout needs to see, and
// ldflags cannot report it because the pipeline believes it is building a clean
// tag.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Injected at link time with -ldflags "-X metrics-system/internal/buildinfo.version=…".
// They are empty in any build that does not set them (go test, go run, a bare
// go build), which is why every field has a runtime fallback.
var (
	version string
	commit  string
	date    string
)

// Info describes the running build. The json tags are stable: three binaries
// expose this over a debug endpoint and operators grep the output.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// resolveCount exists only so a test can observe the compute-once property from
// the outside: the OnceValue closure runs at most once, so the counter it
// increments is 1 no matter how many callers hit Get(). It costs nothing in
// production precisely because that closure runs a single time.
var resolveCount int

// get is memoised because the answer cannot change over a process's lifetime:
// the ldflags variables are set at link time and ReadBuildInfo reads a table
// baked into the binary. Resolving once also means every caller — and there are
// several across the three binaries — sees an identical Info.
var get = sync.OnceValue(func() Info {
	resolveCount++
	bi, ok := debug.ReadBuildInfo()
	return resolve(version, commit, date, bi, ok)
})

// Get returns the resolved build info for the running process. It is safe for
// concurrent use and computes the answer only once.
func Get() Info { return get() }

// commitLen is how far a VCS revision is truncated. A full git SHA is 40 hex
// characters; the first twelve are unambiguous in any repository an operator
// will ever paste one from, and a short hash is what they type anyway.
const commitLen = 12

// resolve merges the two sources of truth into one Info. It is pure — it takes
// the ldflags values and the BuildInfo explicitly rather than reading globals —
// so the resolution rules can be tested without building a binary for each case.
//
// ok reports whether ReadBuildInfo succeeded; it fails in some test binaries.
// When it is false, or bi is nil, only the ldflags values and runtime
// information are available, and resolve must still return a complete Info
// rather than panic.
func resolve(ldVersion, ldCommit, ldDate string, bi *debug.BuildInfo, ok bool) Info {
	// Start from what is knowable without any build metadata, so that every
	// field has a defined value even when both sources are silent.
	info := Info{
		Version:   "dev",
		Commit:    "unknown",
		Date:      "unknown",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	if ok && bi != nil {
		// "(devel)" is what the toolchain writes for a build that is not at a
		// tagged version; it is not a version an operator can act on, so it is
		// treated the same as an absent one.
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			info.Version = v
		}
		if bi.GoVersion != "" {
			info.GoVersion = bi.GoVersion
		}

		settings := make(map[string]string, len(bi.Settings))
		for _, s := range bi.Settings {
			settings[s.Key] = s.Value
		}

		if rev := settings["vcs.revision"]; rev != "" {
			info.Commit = truncateCommit(rev)
		}
		// The dirty bit is read here, before ldflags is applied, and ldflags
		// never overwrites it: a release stamped clean but built from a modified
		// tree must still report dirty. Absent VCS, it stays false rather than
		// guessing.
		info.Dirty = settings["vcs.modified"] == "true"
		if t := settings["vcs.time"]; t != "" {
			info.Date = t
		}
		// Both halves must be present or the platform is meaningless; falling
		// back to the runtime's own GOOS/GOARCH is better than "linux/".
		if goos, hasOS := settings["GOOS"]; hasOS {
			if goarch, hasArch := settings["GOARCH"]; hasArch {
				info.Platform = goos + "/" + goarch
			}
		}
	}

	// ldflags wins last, field by field, so a value the release pipeline set
	// overrides whatever VCS inferred — with the deliberate exception of Dirty,
	// which ldflags does not carry.
	if ldVersion != "" {
		info.Version = ldVersion
	}
	if ldCommit != "" {
		// Truncated on the same rule as the VCS revision. A release pipeline
		// injects the full 40-character SHA; the contract this package offers its
		// callers is "Commit is at most commitLen characters", and one binary
		// printing a long hash and another a short one is a difference an operator
		// would waste time on.
		info.Commit = truncateCommit(ldCommit)
	}
	if ldDate != "" {
		info.Date = ldDate
	}

	return info
}

// truncateCommit shortens a revision to commitLen characters, leaving a hash
// that is already shorter untouched so a caller who injected a short commit via
// ldflags does not see it mangled.
func truncateCommit(rev string) string {
	if len(rev) > commitLen {
		return rev[:commitLen]
	}
	return rev
}

// String renders the build on one line for human eyes, e.g.
// "v0.11.0 (a1b2c3d4e5f6, 2026-07-10T09:00:00Z, go1.26.2, linux/arm64)", with a
// trailing " [dirty]" when the tree was modified. The unknown-commit case still
// produces a readable line because every field always holds a placeholder.
func (i Info) String() string {
	s := fmt.Sprintf("%s (%s, %s, %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
	if i.Dirty {
		s += " [dirty]"
	}
	return s
}

// Short returns just the version, for a --version flag or a startup log line
// that does not want the full provenance.
func (i Info) Short() string { return i.Version }
