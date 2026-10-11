//go:build windows

package launch

import "os/exec"

// Windows lifecycle is owned by the .NET controller's detached process launcher.
func configureDetached(command *exec.Cmd) {}
