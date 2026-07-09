package server

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
)

// Runtime profiling is a production tool, not a development one: the interesting
// heap growth, the goroutine that never exits and the mutex everything queues
// behind all happen under real load. So the endpoints stay in the binary — but
// not on the public port.
//
// /debug/pprof/cmdline prints the process's argv, and this server accepts
// -jwt-hs256-secret on argv. /debug/pprof/heap is a partial dump of everything
// the process is holding, which for a metrics server includes the metrics.
// Neither belongs on the same listener that serves ingest, and neither should be
// reachable at all unless an operator asked for it.
//
// Hence: a separate listener, disabled by default, and a warning when it is
// bound to anything but the loopback interface.

// NewProfilingServer returns an HTTP server exposing only net/http/pprof, bound
// to addr. It is meant for a loopback address; reach it through an SSH tunnel or
// a kubectl port-forward.
func NewProfilingServer(addr string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	srv, err := New(addr, mux, logger)
	if err != nil {
		return nil, err
	}
	if !isLoopback(srv.Addr()) {
		logger.Warn("pprof listener is not on loopback: it exposes the process command line and heap to the network",
			"addr", srv.Addr())
	}
	return srv, nil
}

// isLoopback reports whether every IP the address resolves to is a loopback
// address. An unresolvable or wildcard host counts as not loopback: ":6060"
// listens on every interface, which is exactly the case worth warning about.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	return ip.IsLoopback()
}

// EnableProfileSampling turns on the two profiles that are off by default
// because they cost something to collect.
//
// mutexFraction: 1 records every contention event, 100 records one in a hundred.
// blockRate: nanoseconds of blocking per sampled event; 1_000_000 samples a
// goroutine blocked for a millisecond. Zero leaves either profile disabled.
func EnableProfileSampling(mutexFraction, blockRate int, logger *slog.Logger) {
	if mutexFraction > 0 {
		runtime.SetMutexProfileFraction(mutexFraction)
		logger.Info("mutex profiling enabled", "fraction", mutexFraction)
	}
	if blockRate > 0 {
		runtime.SetBlockProfileRate(blockRate)
		logger.Info("block profiling enabled", "rate_ns", blockRate)
	}
}
