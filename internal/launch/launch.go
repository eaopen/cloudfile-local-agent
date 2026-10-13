// Package launch starts a no-admin per-user helper process. Only local,
// absolute executables chosen by the calling host can be started.
package launch

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// startDetached is provided by the platform files. It starts executable
// detached from the caller and returns the running command, so the caller can
// release the process.
//
// It is per-platform because detaching is: Windows needs creation flags (and a
// retry, because some job objects forbid breakaway), Unix needs a new session.
// stdin becomes the child's standard input; nil discards it.
// See launch_windows.go and launch_other.go.

// Detached starts executable with args and returns immediately, leaving the
// child running after the caller exits.
func Detached(executable string, args ...string) error {
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("absolute local executable required")
	}
	command, err := startDetached(executable, args, nil)
	if err != nil {
		return err
	}
	// Chrome native messaging must not wait for its own updater subprocess.
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
	command, err := startDetached(executable, args, reader)
	if err != nil {
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
