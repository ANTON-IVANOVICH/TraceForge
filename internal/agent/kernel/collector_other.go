//go:build !linux

package kernel

// On anything but Linux there is no /proc/net/snmp, so the collector cannot
// exist. NewCollector fails the way the network collector's Open does under
// CGO_ENABLED=0: with a sentinel the agent reads as "skip this one". It is built
// on every OS — that is what lets the package compile, and the parser tests run,
// on the developer's macOS exactly as they do in Linux CI.
func NewCollector(_ string) (*Collector, error) {
	return nil, ErrUnavailable
}
