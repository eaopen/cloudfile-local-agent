//go:build windows

package main

import (
	"context"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
	"os"
	"path/filepath"
)

func installedEditSigner(instance, deviceID, sessionID string) (editing.CommitChallengeSigner, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bridge := filepath.Join(filepath.Dir(executable), "CloudFile.Agent.Bridge.exe")
	return func(ctx context.Context, challenge editing.DeviceCommitChallenge) (string, error) {
		return editing.SignCommitWithWindowsBridge(ctx, bridge, challenge, instance, deviceID, sessionID)
	}, nil
}
