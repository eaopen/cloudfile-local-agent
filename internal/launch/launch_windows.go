//go:build windows

package launch

import (
	"os/exec"
	"syscall"
)

// Windows process creation flags.
const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
)

// Detached starts executable with args and returns immediately, leaving the
// child running after the caller exits.
func Detached(executable string, args ...string) error {
	if err := start(executable, args, creationFlags(true)); err == nil {
		return nil
	}
	// Some job objects forbid breakaway (CreateProcess then fails); without the
	// flag the child is still detached and windowless, it merely shares the
	// parent's job lifetime.
	return start(executable, args, creationFlags(false))
}

func start(executable string, args []string, flags uint32) error {
	command := exec.Command(executable, args...)
	// Stdout belongs to the Native Messaging pipe, so the child must never
	// inherit it; progress goes to the agent's own update log instead.
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return command.Start()
}

func creationFlags(breakaway bool) uint32 {
	flags := uint32(createNewProcessGroup | detachedProcess)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	return flags
}
