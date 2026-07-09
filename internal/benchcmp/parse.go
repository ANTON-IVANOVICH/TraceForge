// Package benchcmp parses `go test -bench` output and compares two runs for a
// statistically significant difference.
//
// "It was 367 ns/op, now it is 195 ns/op" is not a result. Benchmarks on a real
// machine are noisy — a background build, a thermal throttle, an unlucky memory
// layout — and a single pair of numbers cannot distinguish an optimization from
// a fluctuation. What settles it is running each side many times and asking
// whether the two samples plausibly came from the same distribution.
//
// The test used here is Mann-Whitney U, the same one benchstat uses. It is a
// rank test: it makes no assumption that timings are normally distributed, which
// matters because they are not — timings have a hard floor and a long right tail.
package benchcmp

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Key identifies one measured quantity: a benchmark and one of its units.
// BenchmarkSeriesKey reports ns/op, B/op and allocs/op — three keys, three
// independent comparisons, because an optimization can trade one for another.
type Key struct {
	Name string
	Unit string
}

// Sample is every observation of one Key across the -count=N runs of a file.
type Sample struct {
	Key    Key
	Values []float64
}

// Collection is one benchmark output file.
type Collection struct {
	order   []Key
	samples map[Key]*Sample
	// procs records the -N suffix (GOMAXPROCS) seen per benchmark. Comparing a
	// run at -8 against one at -4 compares two different machines wearing the
	// same name, so the mismatch is surfaced rather than averaged away.
	procs map[string]map[string]bool
}

func newCollection() *Collection {
	return &Collection{samples: make(map[Key]*Sample), procs: make(map[string]map[string]bool)}
}

// Keys returns the measured quantities in the order they were first seen, so the
// report reads in the order the benchmarks ran.
func (c *Collection) Keys() []Key { return c.order }

// Sample returns the observations for key, or nil.
func (c *Collection) Sample(key Key) *Sample { return c.samples[key] }

// GOMAXPROCS returns the suffixes seen for a benchmark name. More than one means
// the file mixes runs that are not comparable.
func (c *Collection) GOMAXPROCS(name string) []string {
	out := make([]string, 0, len(c.procs[name]))
	for p := range c.procs[name] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// MixedGOMAXPROCS returns the benchmark names this file ran at more than one
// GOMAXPROCS value. Their samples were pooled under one name (the -N suffix is
// not part of the identity), which blends two different machines into one median
// — so the caller warns rather than presenting the blend as a comparison.
func (c *Collection) MixedGOMAXPROCS() []string {
	var out []string
	for name, procs := range c.procs {
		if len(procs) > 1 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// benchLine matches "BenchmarkFoo/sub-8 \t 1000000 \t 128.2 ns/op \t 80 B/op".
// The name may contain anything but whitespace; the iteration count is the first
// field after it.
var procSuffix = regexp.MustCompile(`-\d+$`)

// Parse reads `go test -bench` output. Lines it does not recognise (goos:, PASS,
// ok, compiler noise) are skipped rather than rejected, because in practice the
// file is a redirect of a whole test run.
func Parse(r io.Reader) (*Collection, error) {
	c := newCollection()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		// fields[1] is the iteration count. Its presence is what distinguishes a
		// result line from a log line that happens to start with "Benchmark".
		if _, err := strconv.Atoi(fields[1]); err != nil {
			continue
		}

		name := strings.TrimPrefix(fields[0], "Benchmark")
		procs := "1"
		if m := procSuffix.FindString(name); m != "" {
			name = strings.TrimSuffix(name, m)
			procs = strings.TrimPrefix(m, "-")
		}
		if c.procs[name] == nil {
			c.procs[name] = make(map[string]bool)
		}
		c.procs[name][procs] = true

		// The remaining fields are (value, unit) pairs.
		for i := 2; i+1 < len(fields); i += 2 {
			value, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				break // not a measurement; the rest of the line is not ours
			}
			c.add(Key{Name: name, Unit: fields[i+1]}, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read benchmark output: %w", err)
	}
	if len(c.order) == 0 {
		return nil, fmt.Errorf("no benchmark results found (is this `go test -bench` output?)")
	}
	return c, nil
}

func (c *Collection) add(key Key, value float64) {
	s, ok := c.samples[key]
	if !ok {
		s = &Sample{Key: key}
		c.samples[key] = s
		c.order = append(c.order, key)
	}
	s.Values = append(s.Values, value)
}
