//go:build !windows

package launch

import (
	"os/exec"
	"syscall"
)

// Detached starts executable in its own session, so the child survives the
// caller exiting and cannot be reached by the caller's process group signals.
func Detached(executable string, args ...string) error {
	command := exec.Command(executable, args...)
	// Stdout belongs to the Native Messaging pipe; never let the child write
	// into it. Progress goes to the agent's own update log instead.
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}
