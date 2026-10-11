//go:build !windows

package launch

import (
	"os/exec"
	"syscall"
)

// A browser may terminate the Native Host process group on disconnect.
func configureDetached(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
