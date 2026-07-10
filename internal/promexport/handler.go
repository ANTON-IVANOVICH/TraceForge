package promexport

import (
	"bytes"
	"log/slog"
	"net/http"
)

// contentType is the exact media type a Prometheus scraper expects for the
// 0.0.4 text format. The version parameter is what tells the scraper which
// escaping rules apply, so it is not optional.
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves the union of the gatherers at /metrics.
//
// It renders the whole response into a buffer before touching the
// ResponseWriter. Writing incrementally would commit a 200 and a Content-Type
// on the first write, so a family that failed to render halfway through would
// leave the scraper with a truncated body under a success status — a corrupt
// scrape that looks healthy. Buffering makes the response all-or-nothing: a
// family that fails Validate is dropped and logged, and the scraper still
// receives every family that did validate.
func Handler(logger *slog.Logger, gatherers ...Gatherer) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var families []Family
		for _, g := range gatherers {
			families = append(families, g.Gather()...)
		}

		var buf bytes.Buffer
		if err := Write(&buf, families); err != nil {
			// Write already rendered every valid family into buf; err names the
			// first one it had to skip. Serve what validated rather than fail the
			// whole scrape over one bad metric.
			logger.Warn("promexport: skipped an invalid metric family", "error", err)
		}

		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(buf.Bytes()); err != nil {
			logger.Debug("promexport: writing the scrape response failed", "error", err)
		}
	})
}
