//go:build !windows

package config

import (
	"path/filepath"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
)

// preferredRoots is empty outside Windows. The drive preference is a Windows
// deployment decision (fixed data partition versus system drive); macOS and
// Linux installs keep their existing paths.
func preferredRoots() []string {
	return nil
}

// systemRoot is the location inside the shared install folder, and the only
// root used here.
func systemRoot() (string, error) {
	return filepath.Join(home.Dir(), "sessions"), nil
}

// usableWorkspace mirrors the Windows contract so the shared resolution code
// stays identical across platforms, even though no candidate is offered here.
func usableWorkspace(path string) bool {
	return home.Writable(path)
}
