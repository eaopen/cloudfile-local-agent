//go:build darwin && cgo

package editing

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/mackey"
)

// KeychainCommitSigner opens only the confirmed key, and never creates one
// during a commit or falls back to a plaintext private key.
func KeychainCommitSigner(alias, thumbprint, instance, deviceID, sessionID string) CommitChallengeSigner {
	return func(ctx context.Context, challenge DeviceCommitChallenge) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := challenge.Validate(instance, deviceID, sessionID, time.Now()); err != nil {
			return "", err
		}
		key, err := mackey.Open(alias)
		if err != nil {
			return "", err
		}
		defer key.Close()
		actual, err := key.Thumbprint()
		if err != nil || actual != thumbprint {
			return "", errors.New("confirmed Keychain key mismatch")
		}
		message, err := challenge.Message()
		if err != nil {
			return "", err
		}
		return key.SignDigest(sha256.Sum256(message))
	}
}
