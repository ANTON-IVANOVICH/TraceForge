//go:build unix

package mutate

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup makes cancellation reach the whole subtree.
//
// exec.CommandContext's default cancel signals only the `go` process. But `go
// test` compiles a *.test binary and runs it as a grandchild; killing `go`
// reparents that binary to init, where it runs on until its own -test.timeout —
// so Ctrl+C during a mutation run leaves stragglers burning CPU. Putting the
// child in its own process group and signalling the group on cancel kills the
// test binary too. WaitDelay bounds how long CombinedOutput's Wait blocks if a
// grandchild is still holding the output pipe when the group is killed.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
