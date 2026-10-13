//go:build windows

package editing

import (
	"os"
	"path/filepath"
	"syscall"
)

func windowsLock(path string, share uint32) (*os.File, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(pointer, syscall.GENERIC_READ|syscall.GENERIC_WRITE, share, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
func lockSession(path string) (*os.File, error) { return windowsLock(path, 0) }
func holdWindowsActivity() (*os.File, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, "CloudFile", "NativeHost")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return windowsLock(filepath.Join(directory, "activity.lock"), syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE)
}
