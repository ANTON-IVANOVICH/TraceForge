//go:build !unix

package mutate

import "os/exec"

// setProcessGroup is a no-op off Unix: process-group signalling is a POSIX
// notion. Cancellation still stops the `go` process via the context's default
// cancel; only the reparented test binary may linger.
func setProcessGroup(cmd *exec.Cmd) {}
