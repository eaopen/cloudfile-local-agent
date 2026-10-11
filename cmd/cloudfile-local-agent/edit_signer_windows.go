//go:build windows

package main

import (
	"context"
	"errors"
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
		switch challenge.Operation {
		case "commit":
			return editing.SignCommitWithWindowsBridge(ctx, bridge, challenge, instance, deviceID, sessionID)
		case "lease":
			return editing.SignLeaseWithWindowsBridge(ctx, bridge, challenge, instance, deviceID, sessionID)
		default:
			return "", errors.New("unsupported device signature operation")
		}
	}, nil
}
