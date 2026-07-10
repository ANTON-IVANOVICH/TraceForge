package server

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"metrics-system/internal/promexport"
)

// The RED signals — Rate, Errors, Duration — are the three numbers that answer
// "is the service healthy?" for any request/response system. Rate and Errors
// both come out of one counter split by status; Duration is a histogram, never a
// summary: a summary computes its quantiles inside one process and there is no
// way to average a p99 across replicas, while histogram buckets add.
//
// Every label here is bounded on purpose. `route` is the mux's registered
// pattern, not the request path, because `/api/v1/query?name=x` and its million
// siblings must be one series. `method` and `status` are folded to a known set,
// because both arrive from the network.
const (
	// maxHTTPSeries caps each vector. A /metrics endpoint whose cardinality is
	// driven by request input is a memory leak with a scrape interval.
	maxHTTPSeries = 512

	// routeOther is where unrouted requests land. A request that matches nothing
	// (404) or matches a path but not its method (405) has no pattern, and its
	// path came from whoever sent it.
	//
	// A redirect does have one: ServeMux answers `GET /foo` with a redirect to
	// `/foo/` — a 307 on Go 1.26 — and reports the *registered* pattern
	// `GET /foo/`, so redirects are labelled by their target route rather than
	// folded in here. That is fine, since a registered pattern is a bounded set,
	// but it is worth knowing when a route's counter shows a 3xx for a path nobody
	// wrote a handler for.
	routeOther = "other"
)

// HTTPMetrics records the RED signals for the HTTP API.
type HTTPMetrics struct {
	requests *promexport.CounterVec
	duration *promexport.HistogramVec
	inFlight promexport.Gauge
}

// NewHTTPMetrics builds the vectors. Bucket bounds are the Prometheus defaults:
// they straddle the millisecond-to-ten-second range where an HTTP handler either
// works or has already lost the client.
func NewHTTPMetrics() *HTTPMetrics {
	return &HTTPMetrics{
		requests: promexport.NewCounterVec(
			"traceforge_http_requests_total",
			"Total HTTP requests by method, matched route and status code.",
			[]string{"method", "route", "status"},
			maxHTTPSeries,
		),
		duration: promexport.NewHistogramVec(
			"traceforge_http_request_duration_seconds",
			"HTTP request latency in seconds, by method and matched route.",
			[]string{"method", "route"},
			promexport.DefaultBuckets(),
			maxHTTPSeries,
		),
	}
}

// Middleware records every request that reaches mux.
//
// It takes the mux rather than an http.Handler because the route label needs the
// *pattern* a request matched, and by the time the mux has matched it, the
// pattern lives on a request the mux cloned for the handler — not on the one this
// middleware holds. http.ServeMux.Handler answers the same question up front, at
// the cost of a second routing lookup per request. That lookup is a trie walk on
// a handful of nodes; it costs less than the atomic increments that follow it.
func (m *HTTPMetrics) Middleware(mux *http.ServeMux) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pattern := mux.Handler(r)

			route := normalizeRoute(pattern)
			method := normalizeMethod(r.Method)

			m.inFlight.Add(1)
			defer m.inFlight.Add(-1)

			rec := &metricsRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)
			elapsed := time.Since(start)

			m.requests.WithLabelValues(method, route, strconv.Itoa(rec.statusCode())).Inc()

			// A hijacked connection — the WebSocket upgrade at /ws — lives for as
			// long as the browser tab does. Feeding its lifetime into a request
			// latency histogram would move the p99 of the whole server to "one
			// hour" and quietly destroy the only latency signal there is. The
			// request is still counted; only its duration is meaningless.
			if !rec.hijacked {
				m.duration.WithLabelValues(method, route).Observe(elapsed.Seconds())
			}
		})
	}
}

// Gather implements promexport.Gatherer.
func (m *HTTPMetrics) Gather() []promexport.Family {
	families := m.requests.Gather()
	families = append(families, m.duration.Gather()...)
	families = append(families, promexport.Family{
		Name: "traceforge_http_requests_in_flight",
		Help: "HTTP requests currently being served.",
		Type: promexport.TypeGauge,
		Samples: []promexport.Sample{
			{Value: m.inFlight.Load()},
		},
	})
	return families
}

// normalizeRoute maps a mux pattern onto a label value. An unmatched request has
// no pattern, and its path came from whoever sent it, so it is folded into one
// series rather than becoming one.
func normalizeRoute(pattern string) string {
	if pattern == "" {
		return routeOther
	}
	return pattern
}

// normalizeMethod folds the request method into the set the standard library
// recognises. Anything else is a client that made it past the parser with a
// token of its own choosing, and it does not get a series.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return routeOther
	}
}

// metricsRecorder captures the status code and notices a hijack.
//
// It is a second recorder alongside statusRecorder rather than a field added to
// it: the logging middleware wraps this one, so they nest, and a shared mutable
// recorder between two middlewares would couple their lifetimes for no gain.
type metricsRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	hijacked    bool
}

func (r *metricsRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *metricsRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Hijack forwards to the underlying ResponseWriter and remembers that it
// happened. After a hijack nothing writes a status through this recorder, so the
// status reported is the one the protocol implies: 101 Switching Protocols.
func (r *metricsRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter is not a http.Hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, rw, err
}

// statusCode reports the status to label the request with.
func (r *metricsRecorder) statusCode() int {
	if r.hijacked {
		return http.StatusSwitchingProtocols
	}
	return r.status
}
