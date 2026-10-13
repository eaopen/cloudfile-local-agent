package editing

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"
)

// DeviceLeaseBroker is a single-purpose, device-signed Checkout credential
// handoff. It is neither a general resource-secret API nor a file publisher.
type DeviceLeaseBroker struct{ broker *DeviceCommitBroker }

func NewDeviceLeaseBroker(broker *DeviceCommitBroker) (*DeviceLeaseBroker, error) {
	if broker == nil || broker.sign == nil {
		return nil, errors.New("paired device proof required")
	}
	return &DeviceLeaseBroker{broker: broker}, nil
}

func (b *DeviceLeaseBroker) signed(ctx context.Context, purpose string) (map[string]any, error) {
	path := "lease-challenge"
	if purpose == "confirm" {
		path = "lease-confirm-challenge"
	} else if purpose == "renew" {
		path = "lease-renew-challenge"
	} else if purpose != "receive" {
		return nil, errors.New("unsupported signed Checkout operation")
	}
	body := map[string]any{"session_id": b.broker.sessionID, "device_id": b.broker.deviceID}
	var c struct {
		Challenge DeviceCommitChallenge `json:"challenge"`
	}
	if err := b.broker.request(ctx, path, body, &c); err != nil {
		return nil, err
	}
	if err := c.Challenge.ValidateLease(b.broker.instance, b.broker.deviceID, b.broker.sessionID, time.Now()); err != nil {
		return nil, err
	}
	signature, err := b.broker.sign(ctx, c.Challenge)
	if err != nil {
		return nil, errors.New("signed device Checkout proof unavailable")
	}
	body["challenge"] = c.Challenge
	body["signature"] = signature
	return body, nil
}

var positiveLeaseEpoch = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

func ValidCheckoutLease(l Lease) bool {
	if !proofUUID.MatchString(l.GuardID) || !positiveLeaseEpoch.MatchString(l.Generation) ||
		!positiveLeaseEpoch.MatchString(l.CredentialEpoch) || !objectID.MatchString(l.BaseFileID) ||
		!proofSHA.MatchString(l.Token) {
		return false
	}
	if _, err := strconv.ParseUint(l.Generation, 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseUint(l.CredentialEpoch, 10, 64); err != nil {
		return false
	}
	return true
}

func (b *DeviceLeaseBroker) Receive(ctx context.Context) (Lease, error) {
	body, err := b.signed(ctx, "receive")
	if err != nil {
		return Lease{}, err
	}
	var output struct {
		SessionID string `json:"session_id"`
		Lease     Lease  `json:"lease"`
	}
	if err := b.broker.request(ctx, "lease-receive", body, &output); err != nil {
		return Lease{}, err
	}
	if output.SessionID != b.broker.sessionID || !ValidCheckoutLease(output.Lease) {
		return Lease{}, errors.New("bounded device lease result rejected")
	}
	return output.Lease, nil
}

// Confirm must be called only after the Agent has durably saved its Journal.
// A failed acknowledgement does NOT erase the locally saved Checkout proof.
func (b *DeviceLeaseBroker) Confirm(ctx context.Context) error {
	body, err := b.signed(ctx, "confirm")
	if err != nil {
		return err
	}
	var response struct {
		Confirmed bool   `json:"confirmed"`
		SessionID string `json:"session_id"`
	}
	if err := b.broker.request(ctx, "lease-confirm", body, &response); err != nil {
		return err
	}
	if !response.Confirmed || response.SessionID != b.broker.sessionID {
		return errors.New("Checkout handoff acknowledgment unconfirmed")
	}
	return nil
}

// Renew is safe during a long CAD/Office editing session, but only the live
// paired device may extend the original guarded Checkout; it never uploads or
// releases a file. Failure requires an explicit status/error in the UI.
func (b *DeviceLeaseBroker) Renew(ctx context.Context) error {
	body, err := b.signed(ctx, "renew")
	if err != nil {
		return err
	}
	var response struct {
		SessionID string `json:"session_id"`
		Renewed   bool   `json:"renewed"`
	}
	if err := b.broker.request(ctx, "lease-renew", body, &response); err != nil {
		return err
	}
	if !response.Renewed || response.SessionID != b.broker.sessionID {
		return errors.New("guarded Checkout renewal was not confirmed")
	}
	return nil
}
