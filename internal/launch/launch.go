// Package launch starts a no-admin per-user helper process. Only local,
// absolute executables chosen by the calling host can be started.
package launch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func Detached(executable string, args ...string) error {
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("absolute local executable required")
	}
	command := exec.Command(executable, args...)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	// Chrome native messaging must not wait for its own updater subprocess.
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// Binary is compiled for the same user as the host, not as a privileged
// updater service. The caller owns authentication and target validation.
var _ = os.ErrNotExist
