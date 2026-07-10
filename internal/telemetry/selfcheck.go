package telemetry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// selfCheckTimeout bounds the self-probe. It must sit well under the container
// runtime's own HEALTHCHECK timeout, or docker reports a timeout where the
// process would have reported the actual failure.
const selfCheckTimeout = 2 * time.Second

// SelfCheck asks the running process's own /readyz and reports the answer.
//
// It exists because `HEALTHCHECK CMD curl -f localhost:9091/readyz` needs curl,
// and a distroless image contains no curl, no wget, and no shell to invoke them
// with. The two usual answers are to abandon distroless for something with a
// package manager, or to copy a second static binary into the image whose only
// job is to make one HTTP request. The binary was already there; it only needed a
// flag.
//
// addr is the telemetry listen address as configured — typically ":9091". A bare
// port means "every interface" to a listener and nothing at all to a dialer, so
// the host is filled in with loopback. The probe runs inside the container, which
// is the only place loopback is the right answer and the only place this runs.
func SelfCheck(ctx context.Context, addr string) error {
	if addr == "" {
		return fmt.Errorf("-health-check needs -telemetry-addr to be set")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("telemetry address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort(host, port) + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// The body names the failing check. Printing it is the difference between
	// "unhealthy" in `docker ps` and knowing which dependency is down. The reader
	// is capped because the body is only trusted as far as it is small.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz: %s: %s", resp.Status, body)
	}
	return nil
}
