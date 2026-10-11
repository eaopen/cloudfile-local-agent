// Package launch starts a no-admin per-user helper process. Only local,
// absolute executables chosen by the calling host can be started.
package launch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func Detached(executable string, args ...string) error {
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("absolute local executable required")
	}
	command := exec.Command(executable, args...)
	configureDetached(command)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	// Chrome native messaging must not wait for its own updater subprocess.
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// WithInput uses a private pipe instead of passing tickets in process arguments
// or inheriting the browser's native messaging descriptors.
func WithInput(executable string, data []byte, args ...string) error {
	if !filepath.IsAbs(executable) || len(data) > 16384 {
		return errors.New("bounded local worker request required")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()
	command := exec.Command(executable, args...)
	configureDetached(command)
	command.Stdin = reader
	if err := command.Start(); err != nil {
		return err
	}
	reader.Close()
	_ = writer.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = writer.Write(data)
	writer.Close()
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("local worker input failed")
	}
	return command.Process.Release()
}
