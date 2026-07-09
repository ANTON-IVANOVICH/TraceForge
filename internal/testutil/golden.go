package testutil

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// update is registered by this package so every test binary that imports it
// accepts -update. A package must not also declare its own, or the flag package
// panics at init with a duplicate registration.
var update = flag.Bool("update", false, "rewrite golden files with the current output")

// Golden compares got against the contents of testdata/golden/<name>.golden,
// or rewrites that file when -update is passed.
//
//	go test ./internal/cli -update && git diff internal/cli/testdata
//
// The diff is the review artifact: a formatting change you intended shows up as
// the lines you expected, and a formatting change you did not intend shows up as
// lines you have to explain.
func Golden(tb testing.TB, name, got string) {
	tb.Helper()

	path := filepath.Join("testdata", "golden", name+".golden")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			tb.Fatalf("write golden file: %v", err)
		}
		tb.Logf("updated golden file %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read golden file %s: %v (create it with: go test %s -update)", path, err, packageOfTest())
	}
	if string(want) != got {
		tb.Errorf("output does not match %s:\n--- want ---\n%s\n--- got ---\n%s\n%s",
			path, want, got, lineDiff(string(want), got))
	}
}

// GoldenPath is Golden for a caller that keeps its fixtures somewhere other than
// testdata/golden — the e2e suite, for instance.
func GoldenPath(tb testing.TB, path, got string) {
	tb.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			tb.Fatalf("write golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read golden file %s: %v", path, err)
	}
	if string(want) != got {
		tb.Errorf("output does not match %s:\n%s", path, lineDiff(string(want), got))
	}
}

// Updating reports whether -update was passed, for callers that must skip an
// assertion while regenerating fixtures.
func Updating() bool { return *update }

var (
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	hexIDRe     = regexp.MustCompile(`\b[0-9a-f]{16,64}\b`)
	durationRe  = regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|ms|s|m|h)\b`)
)

// Normalize replaces the values that legitimately differ between two runs of the
// same code — timestamps, fingerprints, elapsed times — with placeholders.
//
// Without it a golden file is a flake generator: it pins the clock, and the
// clock always wins. With it, the golden file pins the shape of the output,
// which is the thing under test.
func Normalize(s string) string {
	s = timestampRe.ReplaceAllString(s, "<TIMESTAMP>")
	s = hexIDRe.ReplaceAllString(s, "<ID>")
	s = durationRe.ReplaceAllString(s, "<DURATION>")
	return s
}

// lineDiff renders the first differing line, which is almost always the only one
// a reader needs to see.
func lineDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := "<missing>", "<missing>"
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("first difference at line %d:\n-want: %q\n+got:  %q", i+1, w, g)
		}
	}
	return ""
}

func packageOfTest() string {
	if wd, err := os.Getwd(); err == nil {
		return "./" + filepath.Base(wd)
	}
	return "./..."
}

func sprintf(msg string, args ...any) string {
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
