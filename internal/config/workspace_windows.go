//go:build windows

package config

import (
	"os"
	"path/filepath"
)

func preferredRoots() []string { return []string{`D:\CloudFileAgent2\workspace`} }
func usableWorkspace(root string) bool {
	if root == "" {
		return false
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return false
	}
	file, err := os.CreateTemp(root, ".cf-probe-*")
	if err != nil {
		return false
	}
	file.Close()
	os.Remove(file.Name())
	return true
}
func systemRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CloudFileAgent2", "workspace"), nil
}
