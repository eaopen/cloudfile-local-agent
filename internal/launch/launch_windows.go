//go:build windows

package launch

import (
	"io"
	"os/exec"
	"syscall"
)

// Windows process creation flags.
const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
)

// startDetached starts executable with args and returns the running command,
// leaving the child running after the caller exits.
func startDetached(executable string, args []string, stdin io.Reader) (*exec.Cmd, error) {
	// A background upgrade must break out of the job object Chrome may hold the
	// native host in, and must run without a console window.
	command, err := start(executable, args, stdin, creationFlags(true))
	if err == nil {
		return command, nil
	}
	// Some job objects forbid breakaway (CreateProcess then fails); without the
	// flag the child is still detached and windowless, it merely shares the
	// parent's job lifetime. A fresh command is required: an exec.Cmd cannot be
	// started twice.
	retry, retryErr := start(executable, args, stdin, creationFlags(false))
	if retryErr != nil {
		// Report the original failure: it is the one the flags were meant to fix.
		return nil, err
	}
	return retry, nil
}

func start(executable string, args []string, stdin io.Reader, flags uint32) (*exec.Cmd, error) {
	command := exec.Command(executable, args...)
	// Stdout belongs to the Native Messaging pipe, so the child must never
	// inherit it; progress goes to the agent's own update log instead.
	command.Stdin = stdin
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}

func creationFlags(breakaway bool) uint32 {
	flags := uint32(createNewProcessGroup | detachedProcess)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	return flags
}
