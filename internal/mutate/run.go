package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Verdict is what the test suite said about a mutant.
type Verdict int

const (
	// Killed: the tests failed, which is the outcome you want. The mutation was
	// visible to an assertion.
	Killed Verdict = iota
	// Survived: the tests passed with the bug in place. The mutated line is
	// executed but nothing checks its result.
	Survived
	// Uncompilable: the mutant does not build (`"a" - "b"`). It grades nothing
	// and is excluded from the score rather than counted as a kill, which is
	// what a tool that wants a flattering number would do.
	Uncompilable
	// TimedOut: the mutant made the tests hang — `i < n` becoming `i <= n` in a
	// loop bound. The suite did detect the change, so it counts as a kill.
	TimedOut
)

func (v Verdict) String() string {
	switch v {
	case Killed:
		return "KILLED"
	case Survived:
		return "SURVIVED"
	case Uncompilable:
		return "UNCOMPILABLE"
	case TimedOut:
		return "TIMEOUT"
	default:
		return "UNKNOWN"
	}
}

// Result pairs a mutant with its verdict.
type Result struct {
	Mutant  Mutant
	Verdict Verdict
}

// Config controls a mutation run.
type Config struct {
	Package  string        // the package pattern to mutate and test, e.g. ./internal/alerting/rules
	Dir      string        // module root; the go commands run here
	Timeout  time.Duration // per-mutant test timeout
	Workers  int           // parallel test processes
	Run      string        // optional -run pattern passed to go test
	Verbose  bool
	Progress func(done, total int, r Result)
}

// Run generates every mutant for the package's non-test files, then runs the
// package's tests once per mutant with the mutated file overlaid.
//
// -overlay is what makes this affordable: the toolchain reads one file from a
// different path, so nothing is copied and every mutant shares the module's
// build cache. Without it, each mutant needs a copy of the tree.
func Run(ctx context.Context, cfg Config) ([]Result, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	// A suite that is already red grades nothing: every mutant would be "killed"
	// by the failure that was there before the mutation.
	if err := checkBaseline(ctx, cfg); err != nil {
		return nil, err
	}

	files, err := packageFiles(ctx, cfg)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	type job struct {
		mutant Mutant
		source []byte // the mutated file's full contents
	}
	var jobs []job
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		mutants, err := Generate(fset, path, src)
		if err != nil {
			return nil, err
		}
		for _, m := range mutants {
			mutated, err := m.Apply(src)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, job{mutant: m, source: mutated})
		}
	}
	if len(jobs) == 0 {
		return nil, errors.New("no mutants generated: is the package pattern right?")
	}

	tmp, err := os.MkdirTemp("", "mutate-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	results := make([]Result, len(jobs))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
	)
	sem := make(chan struct{}, cfg.Workers)

	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Result{Mutant: j.mutant, Verdict: Uncompilable}
				return
			}

			verdict, err := testMutant(ctx, cfg, tmp, i, j.mutant, j.source)
			if err != nil && ctx.Err() == nil && cfg.Verbose {
				fmt.Fprintf(os.Stderr, "mutant %d: %v\n", i, err)
			}
			r := Result{Mutant: j.mutant, Verdict: verdict}
			results[i] = r

			mu.Lock()
			done++
			if cfg.Progress != nil {
				cfg.Progress(done, len(jobs), r)
			}
			mu.Unlock()
		}(i, j)
	}
	wg.Wait()

	return results, ctx.Err()
}

// testMutant writes the mutated source and an overlay pointing the original path
// at it, then runs the package's tests.
func testMutant(ctx context.Context, cfg Config, tmpRoot string, id int, m Mutant, src []byte) (Verdict, error) {
	dir := filepath.Join(tmpRoot, fmt.Sprintf("m%d", id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Uncompilable, err
	}
	mutantPath := filepath.Join(dir, filepath.Base(m.File))
	if err := os.WriteFile(mutantPath, src, 0o644); err != nil {
		return Uncompilable, err
	}

	overlay := struct {
		Replace map[string]string
	}{Replace: map[string]string{m.File: mutantPath}}
	overlayBytes, err := json.Marshal(overlay)
	if err != nil {
		return Uncompilable, err
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlayBytes, 0o644); err != nil {
		return Uncompilable, err
	}

	// The timeout is enforced twice: `go test -timeout` makes the test binary
	// panic (which classify sees as TimedOut), and the context is the backstop for
	// a build or a test that hangs past even that. The context gets extra slack so
	// the inner timeout is the one that normally fires.
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout+30*time.Second)
	defer cancel()

	// -json so the verdict comes from the go tool's structured events, not from
	// scanning its text. A mutant's own test output can contain "[build failed]"
	// or "test timed out" — key off those substrings and a real KILL is misfiled
	// as uncompilable (and dropped from the score). The FailedBuild field cannot
	// be forged by test output.
	args := []string{"test", "-json", "-count=1", "-overlay", overlayPath, "-timeout", cfg.Timeout.String()}
	if cfg.Run != "" {
		args = append(args, "-run", cfg.Run)
	}
	args = append(args, cfg.Package)

	cmd := exec.CommandContext(runCtx, "go", args...)
	cmd.Dir = cfg.Dir
	setProcessGroup(cmd) // so cancellation kills the test binary, not just `go`
	out, err := cmd.CombinedOutput()

	if err == nil {
		return Survived, nil
	}
	// The backstop deadline fired: the run hung past cfg.Timeout+slack. The
	// mutation was detected (the tests never finished passing), so it counts as a
	// kill, recorded as a timeout.
	if runCtx.Err() == context.DeadlineExceeded {
		return TimedOut, nil
	}
	return classify(out), nil
}

// classify reads `go test -json` events. A non-zero exit does not distinguish a
// build failure from a test failure — both are exit 1 — so the verdict comes
// from the structured stream: the package-level fail event carries FailedBuild
// only when the mutant did not compile, and the timeout panic is emitted at the
// start of an output line (a user's t.Log of the same text is indented, so it
// does not match).
func classify(out []byte) Verdict {
	var buildFailed, timedOut bool

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue // compiler errors reach CombinedOutput as raw stderr; skip them
		}
		var ev struct {
			Action      string `json:"Action"`
			Output      string `json:"Output"`
			FailedBuild string `json:"FailedBuild"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.FailedBuild != "" {
			buildFailed = true
		}
		if ev.Action == "output" && strings.HasPrefix(ev.Output, "panic: test timed out after") {
			timedOut = true
		}
	}

	switch {
	case buildFailed:
		return Uncompilable
	case timedOut:
		return TimedOut
	default:
		return Killed
	}
}

// checkBaseline runs the unmutated tests once.
func checkBaseline(ctx context.Context, cfg Config) error {
	args := []string{"test", "-count=1", "-timeout", cfg.Timeout.String()}
	if cfg.Run != "" {
		args = append(args, "-run", cfg.Run)
	}
	args = append(args, cfg.Package)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = cfg.Dir
	setProcessGroup(cmd) // baseline runs the real (slow) test suite too; Ctrl+C here must not orphan it
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the unmutated tests must pass before mutants mean anything:\n%s", out)
	}
	return nil
}

// packageFiles asks the toolchain which non-test .go files the package compiles,
// which is more reliable than globbing: it honours build tags and the current
// GOOS/GOARCH, so a mutant is never generated for a file this build ignores.
func packageFiles(ctx context.Context, cfg Config) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}\n{{range .GoFiles}}{{.}}\n{{end}}", cfg.Package)
	cmd.Dir = cfg.Dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", cfg.Package, err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("package %s has no non-test Go files", cfg.Package)
	}
	dir := lines[0]
	files := make([]string, 0, len(lines)-1)
	for _, name := range lines[1:] {
		if name = strings.TrimSpace(name); name != "" {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files, nil
}

// Score summarises a run. Uncompilable mutants are excluded from the
// denominator: they never reached a test, so counting them either way would be a
// lie about the suite.
type Score struct {
	Killed, Survived, Uncompilable, TimedOut int
}

func Summarize(results []Result) Score {
	var s Score
	for _, r := range results {
		switch r.Verdict {
		case Killed:
			s.Killed++
		case Survived:
			s.Survived++
		case Uncompilable:
			s.Uncompilable++
		case TimedOut:
			s.TimedOut++
		}
	}
	return s
}

// Graded is the number of mutants that actually reached the test suite.
func (s Score) Graded() int { return s.Killed + s.TimedOut + s.Survived }

// Percent is the mutation score: the share of graded mutants the suite detected.
func (s Score) Percent() float64 {
	if s.Graded() == 0 {
		return 0
	}
	return float64(s.Killed+s.TimedOut) / float64(s.Graded()) * 100
}
