//go:build darwin && cgo

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
)

// Exactly one front-end consent dialog per registered original device
// Checkout, whether it was opened by a browser or by TaskCenter. This does
// not replace original Intent/Journal locks used inside the upload engine.
func acquireMacSessionDialog(sessionID string) (func(), error) {
	if !device.ID.MatchString(sessionID) {
		return nil, errors.New("original paired session required")
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cfg, "CloudFileAgent2", "dialogs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private dialog state required")
	}
	path := filepath.Join(dir, sessionID+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("a consent window already manages this session")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
