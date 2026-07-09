package cli

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"metrics-system/internal/cli/output"
)

// parseTime accepts an RFC3339 instant, a relative offset such as "-1h" or "-30m",
// or "now". Relative offsets are what people actually type; RFC3339 is what
// scripts generate.
func parseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return time.Time{}, nil
	case "now":
		return now, nil
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid relative time %q: %w", s, err)
		}
		return now.Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q: want RFC3339 (2026-07-09T10:00:00Z) or a relative offset (-1h)", s)
	}
	return t, nil
}

// reservedQueryKeys are the query-string names the API gives its own meaning.
// A label filter may not use one: it would collide with the real parameter, and
// the server could not address such a label anyway.
var reservedQueryKeys = map[string]bool{
	"name": true, "from": true, "to": true, "agg": true, "step": true, "limit": true,
}

// parseLabels turns repeated `key=value` flags into a label filter.
func parseLabels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, Usagef("invalid label %q: want key=value", p)
		}
		if reservedQueryKeys[k] {
			return nil, Usagef("label %q is reserved by the query API", k)
		}
		out[k] = v
	}
	return out, nil
}

// confirm asks for a yes/no answer. Non-interactive callers must pass --yes:
// silently proceeding would make a scripted `rules delete` destroy things its
// author never saw, and blocking on a read would hang the script instead.
func confirm(c *Context, prompt string) (bool, error) {
	if c.AssumeYes {
		return true, nil
	}
	if !interactive(c) {
		return false, Usagef("%s: refusing to continue without a terminal; pass --yes", prompt)
	}
	_, _ = fmt.Fprintf(c.Stdout, "%s [y/N]: ", prompt)

	line, err := bufio.NewReader(c.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil // EOF (Ctrl+D) means "no", not a crash
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// interactive reports whether both stdin and stdout are terminals, which is the
// only situation in which a prompt makes sense.
func interactive(c *Context) bool {
	in, ok := c.Stdin.(*os.File)
	if !ok {
		return false
	}
	return output.IsTerminalFile(in) && output.IsTerminal(c.Stdout)
}

// prompt reads one line of input, returning def when the answer is empty.
func prompt(c *Context, question, def string) (string, error) {
	if def != "" {
		_, _ = fmt.Fprintf(c.Stdout, "%s [%s]: ", question, def)
	} else {
		_, _ = fmt.Fprintf(c.Stdout, "%s: ", question)
	}
	line, err := bufio.NewReader(c.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// labelString renders labels deterministically for a table cell.
func labelString(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// truncate keeps a table cell from wrapping the terminal. It counts and cuts
// runes, not bytes: slicing a byte index through a multi-byte rune would emit
// invalid UTF-8 into the cell.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}
