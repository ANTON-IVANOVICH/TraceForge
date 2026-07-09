// Package receivers delivers alert groups to the outside world. One small
// interface, many implementations — the same shape as the agent's Collector and
// the server's Storage.
package receivers

import (
	"context"
	"errors"
	"fmt"

	"metrics-system/internal/alerting/alert"
)

// Receiver delivers one alert group to a destination. Implementations must
// honour ctx cancellation and must be safe for concurrent use: the notifier
// fans several groups out to the same receiver at once.
type Receiver interface {
	Name() string
	Send(ctx context.Context, g *alert.Group) error
}

// permanentError marks a failure that retrying cannot fix — a malformed
// payload, a rejected credential, a 4xx that is not 408/429. Retrying those
// only burns the retry budget and hammers a service that has already said no.
type permanentError struct{ err error }

func (e permanentError) Error() string { return "permanent: " + e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the retry queue drops it instead of scheduling a retry.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// Permanentf is Permanent with formatting.
func Permanentf(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
