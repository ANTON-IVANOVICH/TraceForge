package output

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// ANSI escape sequences used for status colouring.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiGrey   = "\x1b[90m"
	ansiBold   = "\x1b[1m"
)

// Colorizer paints text, or does nothing. Escape codes written into a pipe or a
// file are noise: `\x1b[31m` in a log, unparseable by the script reading it. So
// colour is enabled only for a real terminal, and any of --no-color, NO_COLOR
// (see no-color.org) or TERM=dumb turns it off.
type Colorizer struct{ enabled bool }

// NewColorizer decides whether w should be coloured.
func NewColorizer(w io.Writer, forceOff bool) *Colorizer {
	return &Colorizer{enabled: !forceOff && shouldColor(w)}
}

func shouldColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(w)
}

// Enabled reports whether colouring is on.
func (c *Colorizer) Enabled() bool { return c != nil && c.enabled }

func (c *Colorizer) paint(code, s string) string {
	if !c.Enabled() {
		return s
	}
	return code + s + ansiReset
}

func (c *Colorizer) Red(s string) string    { return c.paint(ansiRed, s) }
func (c *Colorizer) Green(s string) string  { return c.paint(ansiGreen, s) }
func (c *Colorizer) Yellow(s string) string { return c.paint(ansiYellow, s) }
func (c *Colorizer) Grey(s string) string   { return c.paint(ansiGrey, s) }
func (c *Colorizer) Bold(s string) string   { return c.paint(ansiBold, s) }

// Severity paints an alert severity by urgency.
func (c *Colorizer) Severity(s string) string {
	switch s {
	case "critical":
		return c.Red(s)
	case "warning":
		return c.Yellow(s)
	case "info":
		return c.Grey(s)
	default:
		return s
	}
}

// State paints an alert state.
func (c *Colorizer) State(s string) string {
	switch s {
	case "firing":
		return c.Red(s)
	case "pending":
		return c.Yellow(s)
	case "resolved", "inactive":
		return c.Green(s)
	default:
		return s
	}
}

// IsTerminal reports whether w is an interactive terminal.
//
// The file mode is not enough: /dev/null is a character device too, so a
// mode-only check would colour `metricsctl alerts list > /dev/null` and would
// treat a redirected stdin as a place to prompt. Only the terminal ioctl can
// tell them apart.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// IsTerminalFile reports whether f is an interactive terminal.
func IsTerminalFile(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// Age renders a timestamp the way a human reads it: "2m" beats an ISO-8601
// string that eats thirty columns. Machine output keeps the raw timestamp.
func Age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return Duration(time.Since(t))
}

// Duration renders a duration compactly: 45s, 12m, 3h, 5d.
func Duration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
