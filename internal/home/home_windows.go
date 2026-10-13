//go:build windows

package home

import (
	"syscall"
	"unsafe"
)

// driveFixed is DRIVE_FIXED from the Win32 GetDriveType documentation. Every
// other value (removable, remote, CD-ROM, RAM disk, unknown) disqualifies a
// candidate, which is what keeps an optical or mapped D: from being chosen.
const driveFixed = 3

var procGetDriveTypeW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

// installRoots lists the install folders in preference order. Only D: and C:
// are attempted: probing further letters would make the location unpredictable
// across machines, and the folder is meant to be findable by hand.
func installRoots() []string {
	return []string{`D:\CloudFileLocal`, `C:\CloudFileLocal`}
}

// FixedDrive reports whether the volume holding path is a fixed local disk.
// A missing drive, an optical drive or a mapped share all answer false.
func FixedDrive(path string) bool {
	volume := volumeOf(path)
	if volume == "" {
		return false
	}
	ptr, err := syscall.UTF16PtrFromString(volume)
	if err != nil {
		return false
	}
	value, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(ptr)))
	return uint32(value) == driveFixed
}

// volumeOf normalises a path to the "D:\" form GetDriveTypeW expects.
func volumeOf(path string) string {
	for index := 0; index < len(path); index++ {
		if path[index] == ':' {
			// The path has to start with a letter and a colon.
			if index == 1 && isLetter(path[0]) {
				return path[:2] + `\`
			}
			return ""
		}
		if path[index] == '\\' || path[index] == '/' {
			break
		}
	}
	return ""
}

func isLetter(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
