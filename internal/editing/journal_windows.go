//go:build windows

package editing

import (
	"syscall"
	"unsafe"
)

func renameJournal(from, to string) error {
	source, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// Same-directory replacement with metadata flushed before returning.
	move := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	ok, _, callErr := move.Call(uintptr(unsafe.Pointer(source)), uintptr(unsafe.Pointer(target)), 9)
	if ok == 0 {
		return callErr
	}
	return nil
}
