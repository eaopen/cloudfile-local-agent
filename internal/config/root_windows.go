//go:build windows

package config

import (
	"path/filepath"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
)

// preferredDrive is the drive the mirror is placed on when it is usable.
//
// CAD/GIS drawings are large and the system drive is usually the smallest and
// most contended one, so mirrored downloads are kept off it whenever a real
// data partition is available.
const preferredDrive = `D:\`

// preferredRoots lists the mirror roots to try, in order, before falling back
// to the install folder. Only D: is attempted: a missing or unusable D: goes
// straight to {home}\sessions rather than probing further letters, so the
// resolved location stays predictable across machines.
func preferredRoots() []string {
	return []string{filepath.Join(preferredDrive, "CloudFileLocal", "sessions")}
}

// systemRoot is the location inside the shared install folder. On a machine the
// installer set up on D: this is the same path preferredRoots offers; on one
// that fell back to the per-user folder it is the historical location, so an
// existing mirror is never relocated by surprise.
func systemRoot() (string, error) {
	return filepath.Join(home.Dir(), "sessions"), nil
}

// usableWorkspace reports whether the mirror can really live at path: a fixed
// data volume (an optical or mapped D: must not win) that can be written to.
func usableWorkspace(path string) bool {
	return home.FixedDrive(path) && home.Writable(path)
}
