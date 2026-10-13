//go:build windows

package home

import (
	"os"
	"testing"
)

// A path that cannot name a drive (relative, or a UNC share) must never be
// treated as a fixed local volume.
func TestFixedDriveRejectsPathsWithoutALocalVolume(t *testing.T) {
	for _, path := range []string{`relative\sessions`, `\\server\share\sessions`, `CloudFileLocal`} {
		if FixedDrive(path) {
			t.Fatalf("%q must not report a fixed drive", path)
		}
		if got := volumeOf(path); got != "" {
			t.Fatalf("volumeOf(%q) = %q, want empty", path, got)
		}
	}
}

func TestVolumeOfNormalisesDriveLetters(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{`D:\CloudFileLocal`, `D:\`},
		{`C:\`, `C:\`},
		{`c:\CloudFileLocal\sessions`, `c:\`},
		{`D:relative`, `D:\`},
	} {
		if got := volumeOf(c.path); got != c.want {
			t.Fatalf("volumeOf(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// The drive type is what disqualifies optical, removable and mapped volumes.
// It must be reported for a real root even when the root is not writable.
func TestFixedDriveReportsTheSystemVolume(t *testing.T) {
	root := os.Getenv("SystemDrive") + `\`
	if !FixedDrive(root) {
		t.Skipf("%s is not a fixed local disk on this machine", root)
	}
}
