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
func systemRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CloudFileLocal", "workspace"), nil
}
