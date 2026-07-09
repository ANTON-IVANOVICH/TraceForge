// Package output renders command results. The same result must be printable as
// a table for a human, as JSON for jq, as YAML to paste into a file, and as bare
// names for xargs — otherwise the CLI cannot be part of a pipeline and users end
// up parsing tables with regexes.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Format is an output encoding.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatName  Format = "name"
)

// Formats lists the supported values, for help text and shell completion.
func Formats() []string {
	return []string{string(FormatTable), string(FormatJSON), string(FormatYAML), string(FormatName)}
}

// Table is the human-facing view of a result: a header row and its rows. The
// first column is the resource's name, which is what `-o name` prints.
type Table struct {
	Headers []string
	Rows    [][]string
}

// Printer renders a result.
//
// obj is the raw API object, encoded verbatim by the json and yaml printers, so
// machine-readable output never goes through the lossy table projection. tbl is
// the human projection, used by the table and name printers.
type Printer interface {
	Print(obj any, tbl Table) error
	Format() Format
}

// NewPrinter returns the printer for a format name.
func NewPrinter(format string, w io.Writer) (Printer, error) {
	switch Format(strings.ToLower(strings.TrimSpace(format))) {
	case FormatTable, "":
		return &tablePrinter{w: w}, nil
	case FormatJSON:
		return &jsonPrinter{w: w}, nil
	case FormatYAML:
		return &yamlPrinter{w: w}, nil
	case FormatName:
		return &namePrinter{w: w}, nil
	default:
		return nil, fmt.Errorf("unknown output format %q (want one of: %s)", format, strings.Join(Formats(), ", "))
	}
}

// columnGap is the run of spaces between two columns.
const columnGap = 3

// tablePrinter renders aligned columns. Headers are upper-cased, as in kubectl,
// docker and gh.
//
// It aligns by hand rather than with text/tabwriter because a cell may carry
// ANSI colour: tabwriter counts those escape bytes as visible width, so every
// column after a coloured one drifts. displayWidth ignores them, and counts
// runes rather than bytes so a non-ASCII label does not skew the layout either.
type tablePrinter struct{ w io.Writer }

func (p *tablePrinter) Format() Format { return FormatTable }

func (p *tablePrinter) Print(_ any, tbl Table) error {
	if len(tbl.Rows) == 0 {
		_, err := fmt.Fprintln(p.w, "No resources found.")
		return err
	}

	lines := make([][]string, 0, len(tbl.Rows)+1)
	if len(tbl.Headers) > 0 {
		lines = append(lines, upper(tbl.Headers))
	}
	lines = append(lines, tbl.Rows...)

	widths := make([]int, 0, 8)
	for _, row := range lines {
		for i, cell := range row {
			for i >= len(widths) {
				widths = append(widths, 0)
			}
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	for _, row := range lines {
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 { // never pad the last column: no trailing blanks
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+columnGap))
			}
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(p.w, b.String())
	return err
}

// ansiPattern matches an ANSI SGR escape sequence.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// displayWidth is the number of columns a cell occupies once colour codes are
// removed.
func displayWidth(s string) int {
	return utf8.RuneCountInString(ansiPattern.ReplaceAllString(s, ""))
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// jsonPrinter encodes the raw object. A list stays a list even with one element,
// so `jq '.[0]'` works no matter how many results came back.
type jsonPrinter struct{ w io.Writer }

func (p *jsonPrinter) Format() Format { return FormatJSON }

func (p *jsonPrinter) Print(obj any, _ Table) error {
	enc := json.NewEncoder(p.w)
	enc.SetIndent("", "  ")
	return enc.Encode(obj)
}

type yamlPrinter struct{ w io.Writer }

func (p *yamlPrinter) Format() Format { return FormatYAML }

func (p *yamlPrinter) Print(obj any, _ Table) error {
	// Round-trip through JSON so the server's json tags decide the field names;
	// the API types carry no yaml tags of their own.
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	enc := yaml.NewEncoder(p.w)
	enc.SetIndent(2)
	if err := enc.Encode(generic); err != nil {
		return err
	}
	return enc.Close()
}

// namePrinter prints the first column of each row, one per line, so the output
// pipes straight into xargs.
type namePrinter struct{ w io.Writer }

func (p *namePrinter) Format() Format { return FormatName }

func (p *namePrinter) Print(_ any, tbl Table) error {
	for _, row := range tbl.Rows {
		if len(row) == 0 {
			continue
		}
		if _, err := fmt.Fprintln(p.w, row[0]); err != nil {
			return err
		}
	}
	return nil
}
