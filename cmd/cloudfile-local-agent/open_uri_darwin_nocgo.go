//go:build darwin && !cgo

package main

import "errors"

func runOpenURIFromStdin() error {
	return errors.New("macOS Keychain device support requires cgo build")
}
