package output

import (
	"fmt"
	"io"
	"testing"
)

// Package-level sinks defeat dead-code elimination: without a reachable use of
// each result the whole call collapses and the loop measures nothing.
var (
	sinkInt int
	sinkErr error
)

// tableOf builds a Table with rows realistic columns: a name, a coloured status
// (the ANSI path displayWidth has to strip), an age and a numeric value.
func tableOf(rows int) Table {
	t := Table{Headers: []string{"name", "status", "age", "value"}}
	t.Rows = make([][]string, rows)
	for i := range t.Rows {
		status := ansiGreen + "firing" + ansiReset
		if i%2 == 0 {
			status = ansiRed + "resolved" + ansiReset
		}
		t.Rows[i] = []string{
			fmt.Sprintf("web-server-%d", i),
			status,
			fmt.Sprintf("%dm", i%60),
			fmt.Sprintf("%d", i*7),
		}
	}
	return t
}

// BenchmarkTablePrinter tracks how rendering scales with row count. Layout is
// two passes over every cell (measure widths, then pad), and each cell pays a
// displayWidth call — so a wide table is where this cost lands.
func BenchmarkTablePrinter(b *testing.B) {
	for _, rows := range []int{1, 10, 100, 1000} {
		tbl := tableOf(rows)
		p, _ := NewPrinter("table", io.Discard)
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkErr = p.Print(nil, tbl)
			}
		})
	}
}

// BenchmarkJSONPrinter measures the machine-readable path, which encodes the raw
// object rather than the table projection.
func BenchmarkJSONPrinter(b *testing.B) {
	for _, rows := range []int{1, 10, 100, 1000} {
		obj := make([]sample, rows)
		for i := range obj {
			obj[i] = sample{Name: fmt.Sprintf("web-server-%d", i), Value: i * 7}
		}
		p, _ := NewPrinter("json", io.Discard)
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkErr = p.Print(obj, Table{})
			}
		})
	}
}

// BenchmarkDisplayWidth isolates the per-cell width calculation. It runs once
// for every cell on every render, and the regexp-driven ANSI strip is its
// dominant cost — hence the coloured and plain sub-cases, which take different
// paths through ReplaceAllString.
func BenchmarkDisplayWidth(b *testing.B) {
	cases := map[string]string{
		"plain":   "web-server-42",
		"unicode": "héllo-wörld-café",
		"colored": ansiRed + "resolved" + ansiReset,
		"multi":   ansiBold + ansiGreen + "firing" + ansiReset,
	}
	for name, in := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkInt = displayWidth(in)
			}
		})
	}
}
