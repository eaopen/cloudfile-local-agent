//go:build windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func preferredRoots() []string { return []string{`D:\CloudFileAgent2\workspace`} }

// driveFixed is DRIVE_FIXED from the Win32 GetDriveType API.
const driveFixed = 3

// GetDriveType is not part of Go's syscall package, so it is resolved lazily.
// Doing it here instead of pulling in golang.org/x/sys/windows keeps this
// module free of third-party dependencies.
var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	getDriveType = kernel32.NewProc("GetDriveTypeW")
)

// fixedVolume reports whether path lives on a fixed local disk.
//
// An optical drive, a removable stick, a RAM disk or a mapped network share
// must never become the mirror root: they can be far slower, they can vanish
// mid-download, and the drive policy is re-evaluated on every call -- so a
// flapping volume would move the mirror back and forth.  A UNC path
// (\\server\share\...) never reports DRIVE_FIXED -- DRIVE_REMOTE when the
// share is reachable, DRIVE_NO_ROOT_DIR when it is not -- so it is rejected
// either way.  The probe is a mount-table lookup, not a network round trip:
// measured well under a millisecond for both local and UNC paths.
func fixedVolume(path string) bool {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return false
	}
	pointer, err := syscall.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return false
	}
	kind, _, _ := getDriveType.Call(uintptr(unsafe.Pointer(pointer)))
	return kind == driveFixed
}

// usableWorkspace is the probe the drive policy runs against each candidate.
// Both halves matter: the volume check rejects "D: is a card reader", the
// write probe rejects "D: is read-only or full".
func usableWorkspace(root string) bool {
	if root == "" || !fixedVolume(root) {
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
	// Removed immediately: the directory is listed in the extension popup, so
	// a leftover probe file would be visible to users.
	os.Remove(file.Name())
	return true
}

// systemRoot is the fallback used when no preferred drive is usable.
//
// It is deliberately %LocalAppData% (os.UserCacheDir on Windows) rather than
// %AppData%: a roaming profile is synchronised to the domain server, and a
// file mirror can be large.  %LocalAppData% is still per-user and is not
// touched by Storage Sense's temporary-file cleanup, so the local edit copies
// kept there are not at risk.
func systemRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CloudFileAgent2", "workspace"), nil
}

// supersededRoots lists default locations this agent used before the drive
// policy settled.  The mirror used to fall back to %AppData% (Roaming); Adopt
// still looks there once, so a machine that already ran an older build keeps
// its local edit copies instead of silently starting from an empty mirror.
// Compatibility-only: drop this once no such install is left in the field.
func supersededRoots() []string {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(base, "CloudFileAgent2", "workspace")}
}
