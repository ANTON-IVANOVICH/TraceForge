package output

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type sample struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

var (
	testObj = []sample{{Name: "web-1", Value: 1}, {Name: "web-2", Value: 22}}
	testTbl = Table{
		Headers: []string{"name", "value"},
		Rows:    [][]string{{"web-1", "1"}, {"web-2", "22"}},
	}
)

func TestTablePrinterAlignsAndUpperCases(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, err := NewPrinter("table", &buf)
	if err != nil {
		t.Fatalf("NewPrinter: %v", err)
	}
	if err := p.Print(testObj, testTbl); err != nil {
		t.Fatalf("Print: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header plus two rows:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("header = %q, want it upper-cased (the kubectl convention)", lines[0])
	}
	// Columns are aligned: the second column begins at the same offset on every
	// line, header included. That is the whole point of tabwriter.
	want := secondColumnOffset(lines[0])
	for i, line := range lines {
		if got := secondColumnOffset(line); got != want {
			t.Fatalf("line %d starts its second column at %d, want %d:\n%s", i, got, want, buf.String())
		}
	}
}

// secondColumnOffset returns the index at which the second whitespace-separated
// field begins, or -1.
func secondColumnOffset(line string) int {
	gap := strings.IndexRune(line, ' ')
	if gap < 0 {
		return -1
	}
	rest := line[gap:]
	return gap + (len(rest) - len(strings.TrimLeft(rest, " ")))
}

// A coloured cell must not skew the columns after it: text/tabwriter would
// count the escape bytes as visible width and push everything to the right.
func TestTablePrinterIgnoresAnsiWidth(t *testing.T) {
	t.Parallel()
	var plain, coloured bytes.Buffer
	p1, _ := NewPrinter("table", &plain)
	p2, _ := NewPrinter("table", &coloured)

	rows := Table{Headers: []string{"a", "b"}, Rows: [][]string{{"x", "1"}, {"yy", "2"}}}
	if err := p1.Print(nil, rows); err != nil {
		t.Fatalf("Print: %v", err)
	}

	painted := Table{Headers: []string{"a", "b"}, Rows: [][]string{
		{ansiRed + "x" + ansiReset, "1"},
		{ansiGreen + "yy" + ansiReset, "2"},
	}}
	if err := p2.Print(nil, painted); err != nil {
		t.Fatalf("Print: %v", err)
	}

	// Strip the colour back out: the layout must be byte-identical to the plain one.
	stripped := ansiPattern.ReplaceAllString(coloured.String(), "")
	if stripped != plain.String() {
		t.Fatalf("colour changed the layout:\n--- coloured (stripped) ---\n%s\n--- plain ---\n%s", stripped, plain.String())
	}
}

func TestDisplayWidth(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		"abc":                       3,
		"héllo":                     5, // runes, not bytes
		ansiRed + "abc" + ansiReset: 3,
		"":                          0,
	}
	for in, want := range tests {
		if got := displayWidth(in); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

// An empty result is a success, and says so.
func TestTablePrinterEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, _ := NewPrinter("table", &buf)
	if err := p.Print([]sample{}, Table{Headers: []string{"name"}}); err != nil {
		t.Fatalf("Print: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "No resources found." {
		t.Fatalf("output = %q", got)
	}
}

// The JSON printer encodes the raw object, not the lossy table projection, and a
// list stays a list even with one element so `jq '.[0]'` always works.
func TestJSONPrinterEncodesTheObject(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, _ := NewPrinter("json", &buf)
	if err := p.Print(testObj, Table{}); err != nil {
		t.Fatalf("Print: %v", err)
	}

	var got []sample
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got) != 2 || got[1].Value != 22 {
		t.Fatalf("decoded %+v", got)
	}
	if !strings.Contains(buf.String(), "\n  ") {
		t.Fatal("json output is not indented")
	}
}

func TestJSONPrinterSingleElementStaysAList(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, _ := NewPrinter("json", &buf)
	if err := p.Print([]sample{{Name: "a"}}, Table{}); err != nil {
		t.Fatalf("Print: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Fatalf("output = %q, want a JSON array", buf.String())
	}
}

func TestYAMLPrinterUsesJSONFieldNames(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, _ := NewPrinter("yaml", &buf)
	if err := p.Print(testObj, Table{}); err != nil {
		t.Fatalf("Print: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "name: web-1") || !strings.Contains(out, "value: 22") {
		t.Fatalf("yaml output = %q", out)
	}
}

// -o name prints the first column, one per line, so the output pipes to xargs.
func TestNamePrinter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, _ := NewPrinter("name", &buf)
	if err := p.Print(testObj, testTbl); err != nil {
		t.Fatalf("Print: %v", err)
	}
	if got := buf.String(); got != "web-1\nweb-2\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestUnknownFormat(t *testing.T) {
	t.Parallel()
	if _, err := NewPrinter("xml", &bytes.Buffer{}); err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if _, err := NewPrinter("", &bytes.Buffer{}); err != nil {
		t.Fatalf("an empty format must default to table: %v", err)
	}
}

// Escape codes written into a pipe are noise the reading script cannot parse,
// so a non-terminal writer is never coloured.
func TestColorizerDisabledOffTerminal(t *testing.T) {
	t.Parallel()
	c := NewColorizer(&bytes.Buffer{}, false)
	if c.Enabled() {
		t.Fatal("a bytes.Buffer is not a terminal")
	}
	if got := c.Red("firing"); got != "firing" {
		t.Fatalf("Red = %q, want the bare string", got)
	}
	if got := c.Severity("critical"); got != "critical" {
		t.Fatalf("Severity = %q", got)
	}
}

func TestColorizerForcedOff(t *testing.T) {
	t.Parallel()
	c := NewColorizer(&bytes.Buffer{}, true)
	if c.Enabled() {
		t.Fatal("--no-color must win")
	}
}

// https://no-color.org — any value disables colour everywhere.
func TestNoColorEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if shouldColor(&bytes.Buffer{}) {
		t.Fatal("NO_COLOR must disable colour")
	}
}

// /dev/null is a character device but not a terminal. A mode-only check would
// colour `metricsctl alerts list > /dev/null` and would prompt on a redirected
// stdin; only the terminal ioctl tells them apart.
func TestIsTerminalRejectsCharacterDevices(t *testing.T) {
	t.Parallel()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	if info, err := devNull.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
		t.Skip("this platform does not report /dev/null as a character device")
	}
	if IsTerminal(devNull) {
		t.Fatal("/dev/null is a character device, but it is not a terminal")
	}
	if IsTerminalFile(devNull) {
		t.Fatal("IsTerminalFile must reject /dev/null too")
	}
	if NewColorizer(devNull, false).Enabled() {
		t.Fatal("output redirected to /dev/null must not be coloured")
	}
}

// A regular file is never a terminal either.
func TestIsTerminalRejectsRegularFile(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Fatal("a regular file is not a terminal")
	}
}

func TestDurationAndAge(t *testing.T) {
	t.Parallel()
	tests := map[time.Duration]string{
		45 * time.Second:  "45s",
		12 * time.Minute:  "12m",
		3 * time.Hour:     "3h",
		50 * time.Hour:    "2d",
		-90 * time.Second: "1m",
	}
	for d, want := range tests {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := Age(time.Time{}); got != "-" {
		t.Fatalf("Age(zero) = %q, want -", got)
	}
	if got := Age(time.Now().Add(-2 * time.Hour)); got != "2h" {
		t.Fatalf("Age = %q", got)
	}
}
