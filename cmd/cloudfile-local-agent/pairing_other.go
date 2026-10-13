//go:build !darwin || !cgo

package main

import "errors"

func runPairing(action, instance, alias string) error {
	return errors.New("pairing requires the macOS Keychain-enabled build; Windows uses its .NET controller")
}
