//go:build linux

package kernel

import "os"

// The canonical procfs locations. Confined to the Linux build because they are
// meaningless anywhere else; the portable Collect reads whatever paths the
// Collector was handed, which on production is exactly these two.
const (
	procNetSNMP    = "/proc/net/snmp"
	procNetNetstat = "/proc/net/netstat"
)

// NewCollector returns a procfs-backed collector, or ErrUnavailable when
// /proc/net/snmp cannot be read. Statting the file once here — at startup, not
// on every tick — lets the agent decide whether to include this collector before
// its first collection, instead of logging a failed Collect every interval on a
// host that will never have the file.
//
// Only snmp gates availability. netstat is checked lazily in Collect and its
// absence merely drops two metrics, so a stripped kernel with snmp but no
// netstat still reports the TCP and UDP counters it does have.
func NewCollector(hostname string) (*Collector, error) {
	if _, err := os.Stat(procNetSNMP); err != nil {
		return nil, ErrUnavailable
	}
	return &Collector{
		hostname:    hostname,
		snmpPath:    procNetSNMP,
		netstatPath: procNetNetstat,
	}, nil
}
