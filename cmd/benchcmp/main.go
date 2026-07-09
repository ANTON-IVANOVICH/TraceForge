// Command benchcmp compares two `go test -bench` runs and reports which
// differences are statistically significant.
//
//	go test -run XXX -bench . -benchmem -count=10 ./... > old.txt
//	# ...change something...
//	go test -run XXX -bench . -benchmem -count=10 ./... > new.txt
//	benchcmp old.txt new.txt
//
// -count=10 is not decoration. With one run per side there is no sample, no
// variance and no test — only two numbers and a hope.
//
// It is a small, dependency-free stand-in for golang.org/x/perf/cmd/benchstat,
// which does more (geomeans, multi-way configuration matrices, CSV) and which
// you should reach for once you need any of that.
package main

import (
	"flag"
	"fmt"
	"os"

	"metrics-system/internal/benchcmp"
)

func main() {
	alpha := flag.Float64("alpha", 0.05, "significance level: deltas with p >= alpha are reported as ~")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: benchcmp [-alpha 0.05] old.txt new.txt\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if *alpha <= 0 || *alpha >= 1 {
		fmt.Fprintf(os.Stderr, "benchcmp: -alpha must be in (0, 1), got %v\n", *alpha)
		os.Exit(2)
	}

	oldC, err := parseFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchcmp: %v\n", err)
		os.Exit(1)
	}
	newC, err := parseFile(flag.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchcmp: %v\n", err)
		os.Exit(1)
	}

	// A file that mixes -8 and -4 runs of one benchmark pooled them into a single
	// median. Warn before the table, so the blended row is not read as a
	// like-for-like comparison.
	warnMixedProcs(flag.Arg(0), oldC)
	warnMixedProcs(flag.Arg(1), newC)

	rows, onlyOld, onlyNew := benchcmp.Compare(oldC, newC, *alpha)
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "benchcmp: the two files share no benchmark")
		os.Exit(1)
	}
	if err := benchcmp.Render(os.Stdout, rows, onlyOld, onlyNew); err != nil {
		fmt.Fprintf(os.Stderr, "benchcmp: %v\n", err)
		os.Exit(1)
	}
}

func warnMixedProcs(path string, c *benchcmp.Collection) {
	for _, name := range c.MixedGOMAXPROCS() {
		fmt.Fprintf(os.Stderr, "benchcmp: warning: %s: %s ran at multiple GOMAXPROCS %v; its rows blend them\n",
			path, name, c.GOMAXPROCS(name))
	}
}

func parseFile(path string) (*benchcmp.Collection, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	c, err := benchcmp.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
