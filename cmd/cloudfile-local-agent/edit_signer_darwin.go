//go:build darwin && cgo

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
	"os"
	"path/filepath"
)

func installedEditSigner(instance, deviceID, sessionID string) (editing.CommitChallengeSigner, error) {
	dir, err := pairingDirectory()
	if err != nil {
		return nil, err
	}
	name := sha256.Sum256([]byte(instance))
	path := filepath.Join(dir, hex.EncodeToString(name[:])+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > componentBudget {
		return nil, errors.New("confirmed local pairing required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pair macPairing
	if strictComponentJSON(data, &pair) != nil || pair.Instance != instance || pair.DeviceID != deviceID || pair.ExpiresAt != 0 {
		return nil, errors.New("confirmed device pairing mismatch")
	}
	return editing.KeychainCommitSigner(pair.Alias, pair.Thumbprint, instance, deviceID, sessionID), nil
}
