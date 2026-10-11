//go:build !darwin

package main

import "errors"

func runOpenURIFromStdin() error {
	return errors.New("paired cloudfile-open URI requires native macOS controller")
}
