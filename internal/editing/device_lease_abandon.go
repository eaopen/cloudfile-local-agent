package editing

import (
	"context"
	"errors"
	"time"
)

// AbandonCheckout releases ONLY the original guard after a fresh P-256
// challenge and exact Checkout credential proof. No file bytes are sent and
// a pending publish cannot be silently abandoned.
func (b *DeviceCommitBroker) AbandonCheckout(ctx context.Context, lease Lease) error {
	if b == nil || !proofUUID.MatchString(lease.GuardID) ||
		!proofSHA.MatchString(lease.Token) || !objectID.MatchString(lease.BaseFileID) ||
		lease.Generation == "" || lease.CredentialEpoch == "" {
		return errors.New("original active Checkout proof required")
	}
	body := map[string]any{
		"session_id": b.sessionID, "device_id": b.deviceID,
		"guard_id": lease.GuardID, "generation": lease.Generation,
		"credential_epoch": lease.CredentialEpoch, "base_file_id": lease.BaseFileID,
		"token": lease.Token,
	}
	var challenge struct {
		Challenge DeviceCommitChallenge `json:"challenge"`
	}
	if err := b.request(ctx, "lease-abandon-challenge", body, &challenge); err != nil {
		return err
	}
	c := challenge.Challenge
	if err := c.ValidateLease(b.instance, b.deviceID, b.sessionID, time.Now()); err != nil {
		return err
	}
	signature, err := b.sign(ctx, c)
	if err != nil {
		return errors.New("paired device Checkout release signing failed")
	}
	body["challenge"] = c
	body["signature"] = signature
	var result struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
		Released  bool   `json:"released"`
	}
	if err := b.request(ctx, "lease-abandon", body, &result); err != nil {
		return err
	}
	if result.SessionID != b.sessionID || result.State != "abandoned" || !result.Released {
		return errors.New("checkout release not confirmed by current server")
	}
	return nil
}
