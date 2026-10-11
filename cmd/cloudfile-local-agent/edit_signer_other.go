//go:build !windows && (!darwin || !cgo)

package main

import (
	"errors"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

func installedEditSigner(instance, deviceID, sessionID string) (editing.CommitChallengeSigner, error) {
	return nil, errors.New("native device signing unavailable")
}
