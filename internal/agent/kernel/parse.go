package kernel

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// procStats holds the counters from one /proc/net file, keyed first by protocol
// prefix ("Tcp", "Udp", "TcpExt") and then by field name.
//
// Values are int64, not uint64, because these files carry signed numbers.
// MaxConn prints -1 for "no connection limit", and a 32-bit counter that wrapped
// or a kernel bug can print a negative where a monotonic counter is meant.
// Keeping the raw signed value lets the metric layer decide field by field what a
// negative means, instead of laundering every one of them through a uint here and
// turning -1 into 18 quintillion.
type procStats map[string]map[string]int64

// get returns a field's value and whether it was present. The bool matters: a
// field the running kernel does not emit is absent, which is not the same as a
// field that is present and zero, and the two must not collapse into one metric.
func (s procStats) get(prefix, field string) (int64, bool) {
	fields, ok := s[prefix]
	if !ok {
		return 0, false
	}
	v, ok := fields[field]
	return v, ok
}

// parseProcNet reads the two-line-per-protocol format shared by /proc/net/snmp
// and /proc/net/netstat. Each protocol prints a header line naming its columns
// and a value line with the numbers, both tagged with the same prefix:
//
//	Tcp: RtoAlgorithm RtoMin ... RetransSegs InErrs OutRsts InCsumErrors
//	Tcp: 1 200 ... 7 0 2 0
//
// The column set shifts between kernel versions — a field added upstream lands
// wherever its patch put it — so the header line is the only authority on which
// column is which. Each header is paired with the next value line of the same
// prefix and zipped by position, which *is* parsing by name, since the header
// supplies the names. Nothing here assumes RetransSegs is column eleven; that
// assumption is precisely what breaks on the next kernel.
//
// It never panics and never rejects a file over its content: a malformed line is
// dropped, not guessed at, and an unreadable stream returns the reader's error
// alongside whatever parsed before it. The bytes come from the kernel — but the
// fuzz target feeds this the bytes that do not.
func parseProcNet(r io.Reader) (procStats, error) {
	stats := make(procStats)
	headers := make(map[string][]string)

	sc := bufio.NewScanner(r)
	// A netstat TcpExt line lists ~130 columns; the 64 KiB default token cap
	// clears that comfortably, but a crafted single line could blow past it and
	// turn the whole parse into a "token too long" error. Raising the cap keeps
	// one long line from voiding the file.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue // blank line, or a line of only whitespace
		}
		// The kernel writes every section as "Tcp:", "TcpExt:", "Udp:". A token
		// without the colon is not a section header, and a bare ":" names no
		// protocol at all — the first would be filed under a wrong prefix, the
		// second under an empty one. A fuzzer found the second.
		prefix, ok := strings.CutSuffix(fields[0], ":")
		if !ok || prefix == "" {
			continue
		}
		rest := fields[1:]

		cols, seen := headers[prefix]
		if !seen {
			// First line for a prefix is its header: keep a private copy of the
			// column names, since strings.Fields aliases the scanner's buffer,
			// which the next Scan overwrites.
			names := make([]string, len(rest))
			copy(names, rest)
			headers[prefix] = names
			continue
		}

		// Second line is the values. A length mismatch means header and value
		// lines disagree on the column count — the file was truncated mid-line or
		// otherwise corrupted. There is no way to know which column was lost, so
		// zipping by position would silently file one field's number under
		// another's name. Drop the whole pairing: a missing metric beats a lying
		// one.
		if len(rest) == len(cols) {
			row := make(map[string]int64, len(cols))
			for i, name := range cols {
				v, err := strconv.ParseInt(rest[i], 10, 64)
				if err != nil {
					continue // a non-numeric column: skip it, trust its siblings
				}
				row[name] = v
			}
			// Last pairing wins if a section repeats, matching the kernel's own
			// "latest snapshot" semantics for a re-emitted block.
			stats[prefix] = row
		}
		// Reset either way so a repeated header/value pair for this prefix is read
		// as a fresh pair rather than mistaken for a third value line.
		delete(headers, prefix)
	}

	return stats, sc.Err()
}
