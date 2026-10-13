//go:build !darwin || !cgo

package main

import (
	"errors"
	"io"
)

// Windows uses the existing .NET controller and its explicit per-file forms.
func runManageLocalTask(_ io.Reader) error {
	return errors.New("macOS local editor is unavailable on this platform")
}
