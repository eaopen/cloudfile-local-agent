//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The mirror directory must be creatable, and the write probe must not leave
// anything behind -- the directory is listed in the extension popup, so a
// stray probe file would be visible to users.
func TestUsableWorkspaceCreatesDirectoryAndCleansUpProbe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "CloudFileAgent2", "workspace")
	if !usableWorkspace(dir) {
		t.Skipf("temporary directory is not writable here: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("workspace directory must exist: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe must be removed, found %d entries", len(entries))
	}
}

// The drive policy only accepts fixed local disks.  A UNC path never reports
// DRIVE_FIXED, and a mapped share that happens to be writable must still not
// become the mirror root: it can vanish mid-download, and since the policy is
// re-evaluated on every call it would make the mirror flap.
func TestFixedVolumeRejectsNetworkAndEmptyPaths(t *testing.T) {
	for _, path := range []string{
		`\\server\share\CloudFileAgent2\workspace`,
		`\\server\share`,
		"",
	} {
		if fixedVolume(path) {
			t.Fatalf("%q must not be accepted as a fixed local volume", path)
		}
	}
}

func TestFixedVolumeAcceptsTheTemporaryDirectoryWhenItIsLocal(t *testing.T) {
	dir := t.TempDir()
	if !fixedVolume(dir) {
		t.Skipf("temporary directory is not on a fixed local disk here: %s", dir)
	}
}

// A rejected candidate is refused before anything is created, so probing can
// never leave an empty directory behind on a share.
func TestUsableWorkspaceRefusesNetworkPath(t *testing.T) {
	if usableWorkspace(`\\server\share\CloudFileAgent2\workspace`) {
		t.Fatal("a network path must not be usable")
	}
}
