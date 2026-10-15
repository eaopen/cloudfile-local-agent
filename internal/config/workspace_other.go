//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

func preferredRoots() []string { return nil }
func usableWorkspace(root string) bool {
	if root == "" {
		return false
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return false
	}
	return true
}

// systemRoot is the only root outside Windows.  The mirror is deliberately kept
// in the user config directory (macOS ~/Library/Application Support,
// $XDG_CONFIG_HOME on Linux) rather than the cache directory: nothing roams
// these paths, and unlike ~/Library/Caches or $XDG_CACHE_HOME they are not
// purged by cleanup tools -- which matters, because the mirror holds the user's
// only copy of a local edit.
func systemRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CloudFileAgent2", "workspace"), nil
}

// No non-Windows build ever used a different default root, so there is nothing
// to carry over from a superseded location.
func supersededRoots() []string { return nil }
