//go:build !windows

package launch

import (
	"io"
	"os/exec"
	"syscall"
)

// startDetached starts executable in its own session, so the child survives the
// caller exiting and cannot be reached by the caller's process group signals.
func startDetached(executable string, args []string, stdin io.Reader) (*exec.Cmd, error) {
	command := exec.Command(executable, args...)
	// Stdout belongs to the Native Messaging pipe; never let the child write
	// into it. Progress goes to the agent's own update log instead.
	command.Stdin = stdin
	command.Stdout = nil
	command.Stderr = nil
	// A browser may terminate the Native Host process group on disconnect.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}
