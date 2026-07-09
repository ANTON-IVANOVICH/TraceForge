// Command mutate runs mutation testing over a package.
//
//	mutate ./internal/alerting/rules
//	mutate -min-score 80 -workers 8 ./internal/server/storage
//
// It edits one token at a time — `>` into `>=`, `&&` into `||`, `true` into
// `false` — and runs the package's tests against each edit. A mutant the tests
// still pass is a line they execute without checking.
//
// It is slow by construction: one `go test` per mutant. Run it on the packages
// whose correctness you actually depend on, on a schedule, not on every push.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"syscall"
	"time"

	"metrics-system/internal/mutate"
)

func main() {
	var (
		timeout  = flag.Duration("timeout", 60*time.Second, "per-mutant test timeout")
		workers  = flag.Int("workers", runtime.NumCPU()/2, "parallel test processes")
		run      = flag.String("run", "", "only run tests matching this pattern (passed to go test -run)")
		minScore = flag.Float64("min-score", 0, "exit non-zero if the mutation score falls below this percentage")
		verbose  = flag.Bool("v", false, "print every mutant, not only the survivors")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mutate [flags] <package>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *workers < 1 {
		*workers = 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mutate: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	results, err := mutate.Run(ctx, mutate.Config{
		Package: flag.Arg(0),
		Dir:     wd,
		Timeout: *timeout,
		Workers: *workers,
		Run:     *run,
		Verbose: *verbose,
		Progress: func(done, total int, r mutate.Result) {
			if *verbose || r.Verdict == mutate.Survived {
				fmt.Printf("[%d/%d] %-12s %s\n", done, total, r.Verdict, r.Mutant)
				return
			}
			fmt.Fprintf(os.Stderr, "\r[%d/%d] mutants tested", done, total)
		},
	})
	fmt.Fprint(os.Stderr, "\r")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mutate: %v\n", err)
		os.Exit(1)
	}

	survivors := make([]mutate.Result, 0)
	for _, r := range results {
		if r.Verdict == mutate.Survived {
			survivors = append(survivors, r)
		}
	}
	sort.Slice(survivors, func(i, j int) bool {
		a, b := survivors[i].Mutant, survivors[j].Mutant
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})

	if len(survivors) > 0 {
		fmt.Printf("\n%d surviving mutant(s) — lines your tests run but do not check:\n\n", len(survivors))
		for _, r := range survivors {
			fmt.Printf("  %s\n", r.Mutant)
		}
	}

	s := mutate.Summarize(results)
	fmt.Printf("\nmutation score: %.1f%% (%d killed, %d timed out, %d survived; %d uncompilable, excluded) in %s\n",
		s.Percent(), s.Killed, s.TimedOut, s.Survived, s.Uncompilable, time.Since(start).Round(time.Second))

	if *minScore > 0 && s.Percent() < *minScore {
		fmt.Fprintf(os.Stderr, "mutate: score %.1f%% is below the -min-score of %.1f%%\n", s.Percent(), *minScore)
		os.Exit(1)
	}
}
