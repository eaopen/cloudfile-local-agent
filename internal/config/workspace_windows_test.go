//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The mirror must be creatable on a fixed local volume, and the write probe
// must not leave anything behind -- the directory is listed in the extension
// popup, so a stray probe file would be visible to users.
func TestUsableWorkspaceCreatesSessionsDirectoryOnFixedVolume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "CloudFileLocal", "sessions")
	if !usableWorkspace(dir) {
		t.Skipf("temporary volume is not a fixed local disk here: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("workspace directory must exist: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe must be removed, found %d entries", len(entries))
	}
}
