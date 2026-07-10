package network

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"metrics-system/internal/model"
)

// Collector turns a packet capture into counters.
//
// Capture is push-shaped: packets arrive when the network decides. The agent is
// pull-shaped: every collector is asked for its metrics on a tick. Bridging the
// two by having Collect read packets would make the agent's tick as slow as the
// network is quiet.
//
// So the capture runs in its own goroutine, incrementing atomics, and Collect
// merely reads them. That keeps Collect at a few nanoseconds and lets this
// collector sit beside the CPU and memory ones without changing the agent.
//
// The counters are monotonic totals, not rates. Computing a rate is the query
// layer's job (`rate(net_packets_total[5m])`), and a counter that resets on a
// scrape is a counter that cannot survive a missed scrape.
type Collector struct {
	capture *Capture
	link    LinkType
	device  string
	logger  *slog.Logger

	packets   atomic.Uint64
	bytes     atomic.Uint64
	tcp       atomic.Uint64
	udp       atomic.Uint64
	icmp      atomic.Uint64
	other     atomic.Uint64
	ipv4      atomic.Uint64
	ipv6      atomic.Uint64
	unparsed  atomic.Uint64
	readErrs  atomic.Uint64
	stopped   atomic.Bool
	running   atomic.Bool
	loopEnded chan struct{}

	// Kernel drop counters, sampled by Run and read by Collect.
	//
	// Collect must not call Capture.Stats itself: that enters libpcap, and Run is
	// already inside it. A pcap_t is not thread-safe, so the only goroutine
	// allowed to touch this capture is the one that owns it. Everything Collect
	// reports comes from atomics this loop wrote.
	statsValid  atomic.Bool
	kernelDrops atomic.Uint64
	ifaceDrops  atomic.Uint64
}

// NewCollector opens a capture and returns a collector for it. The caller runs
// Run in a goroutine and Close when done.
func NewCollector(cfg Config, logger *slog.Logger) (*Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}

	capture, err := Open(cfg)
	if err != nil {
		return nil, err
	}

	return &Collector{
		capture:   capture,
		link:      capture.LinkType(),
		device:    capture.Device(),
		logger:    logger,
		loopEnded: make(chan struct{}),
	}, nil
}

// Name identifies the collector in the agent's logs.
func (c *Collector) Name() string { return "network" }

// LinkType reports the link-layer format the capture negotiated.
func (c *Collector) LinkType() LinkType { return c.link }

// Run reads packets until ctx is cancelled, the savefile ends, or the capture
// is closed. It is meant to run in its own goroutine for the agent's lifetime.
func (c *Collector) Run(ctx context.Context) {
	c.running.Store(true)
	defer close(c.loopEnded)

	// Breaking the capture on cancellation is what unblocks a Next that is
	// sitting inside libpcap; without it a live capture on a silent interface
	// holds this goroutine until its next timeout.
	//
	// The watcher exits with Run. Waiting only on ctx.Done() would leak it for
	// the life of the process every time Run returned first — which is what
	// happens at the end of every savefile.
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			c.capture.Break()
		case <-watcherDone:
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		pkt, err := c.capture.Next()
		switch {
		case err == nil:
			c.record(pkt)
			// Sampling every packet would spend a CGo call per packet on a number
			// that changes slowly. Once every statsEvery packets is frequent
			// enough for a counter read on a seconds-long tick.
			if c.packets.Load()%statsEvery == 0 {
				c.sampleStats()
			}

		case errors.Is(err, ErrTimeout):
			// A quiet interface, not a problem. The loop turns so ctx can be
			// checked — and an idle interface is the best moment to refresh the
			// drop counters, since nothing else is happening.
			c.sampleStats()
			continue

		case errors.Is(err, ErrEndOfCapture), errors.Is(err, ErrCaptureClosed):
			return

		default:
			// A read error on one packet is not a reason to stop monitoring.
			// Count it, breathe, and carry on — but do not spin at full speed
			// if the error is permanent.
			c.readErrs.Add(1)
			c.logger.Debug("packet read failed", "device", c.device, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// statsEvery bounds how often Run pays a CGo call to refresh the kernel drop
// counters.
const statsEvery = 1024

// sampleStats refreshes the drop counters. It runs only on Run's goroutine, so
// it is the sole entry into libpcap's stats. A savefile has none; the first
// failure marks them invalid and Collect stops reporting them.
func (c *Collector) sampleStats() {
	_, dropped, ifDropped, err := c.capture.Stats()
	if err != nil {
		c.statsValid.Store(false)
		return
	}
	c.kernelDrops.Store(dropped)
	c.ifaceDrops.Store(ifDropped)
	c.statsValid.Store(true)
}

// record classifies one packet. It is the only writer to the counters, but
// Collect reads them concurrently, so they are atomics rather than plain ints.
func (c *Collector) record(pkt Packet) {
	c.packets.Add(1)
	// WireLength, not len(Data): the snapshot length truncates the copy, and
	// counting the copy would under-report throughput by however much was cut.
	c.bytes.Add(uint64(pkt.WireLength))

	info, ok := Parse(c.link, pkt.Data)
	if !ok {
		c.unparsed.Add(1)
		return
	}

	switch info.Version {
	case 4:
		c.ipv4.Add(1)
	case 6:
		c.ipv6.Add(1)
	}

	switch ProtocolName(info.Protocol) {
	case "tcp":
		c.tcp.Add(1)
	case "udp":
		c.udp.Add(1)
	case "icmp":
		c.icmp.Add(1)
	default:
		c.other.Add(1)
	}
}

// Collect snapshots the counters. It never touches the capture, so it cannot
// block behind a packet read.
func (c *Collector) Collect(_ context.Context) ([]model.Metric, error) {
	now := time.Now().UTC()
	labels := func(extra ...string) map[string]string {
		m := map[string]string{"device": c.device}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}

	counter := func(name string, v uint64, l map[string]string) model.Metric {
		return model.Metric{
			Name:      name,
			Type:      model.MetricTypeCounter,
			Value:     float64(v),
			Timestamp: now,
			Labels:    l,
		}
	}

	metrics := []model.Metric{
		counter("net_packets_total", c.packets.Load(), labels()),
		counter("net_bytes_total", c.bytes.Load(), labels()),
		counter("net_protocol_packets_total", c.tcp.Load(), labels("protocol", "tcp")),
		counter("net_protocol_packets_total", c.udp.Load(), labels("protocol", "udp")),
		counter("net_protocol_packets_total", c.icmp.Load(), labels("protocol", "icmp")),
		counter("net_protocol_packets_total", c.other.Load(), labels("protocol", "other")),
		counter("net_ip_packets_total", c.ipv4.Load(), labels("version", "4")),
		counter("net_ip_packets_total", c.ipv6.Load(), labels("version", "6")),
		counter("net_unparsed_packets_total", c.unparsed.Load(), labels()),
		counter("net_read_errors_total", c.readErrs.Load(), labels()),
	}

	// Kernel drops are the metric that keeps the others honest: under load the
	// kernel discards packets before this process ever sees them, so
	// net_packets_total silently under-counts exactly when the network is
	// busiest.
	//
	// These come from atomics Run sampled, not from a fresh call into libpcap:
	// Collect runs on the agent's tick goroutine while Run sits inside
	// pcap_next_ex, and one pcap_t must have one caller. A savefile has no such
	// statistics, so they are absent rather than a fabricated zero.
	if c.statsValid.Load() {
		metrics = append(metrics,
			counter("net_kernel_dropped_total", c.kernelDrops.Load(), labels()),
			counter("net_interface_dropped_total", c.ifaceDrops.Load(), labels()),
		)
	}

	return metrics, nil
}

// Close stops the capture and waits for Run to leave libpcap. It is idempotent.
func (c *Collector) Close() error {
	if c.stopped.Swap(true) {
		return nil
	}
	err := c.capture.Close()

	// Run may still be inside Next; Close broke the loop, so it returns
	// promptly. Waiting here means the agent never exits with a goroutine still
	// holding a freed C handle. A collector whose Run was never started has
	// nothing to wait for.
	if c.running.Load() {
		select {
		case <-c.loopEnded:
		case <-time.After(2 * time.Second):
		}
	}
	return err
}
