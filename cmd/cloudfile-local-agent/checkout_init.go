package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

// The Windows native GUI or macOS trusted local controller supplies only
// the approved server-origin + existing session/device IDs. There are no
// client-supplied file paths or Checkout credentials in this CLI request.
type checkoutInitRequest struct {
	Instance      string `json:"instance"`
	DeviceID      string `json:"device_id"`
	SessionID     string `json:"session_id"`
	UserConfirmed bool   `json:"user_confirmed"`
}

func runCheckoutInit(input io.Reader, output io.Writer) error {
	if os.Getenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED") != "1" {
		return errors.New("exclusive edit handoff is disabled")
	}
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) > componentBudget {
		return errors.New("bounded local checkout request required")
	}
	var req checkoutInitRequest
	if strictComponentJSON(data, &req) != nil || device.Origin(req.Instance) != nil ||
		!device.ID.MatchString(req.SessionID) || !device.ID.MatchString(req.DeviceID) ||
		!req.UserConfirmed {
		return errors.New("explicit paired device Checkout required")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	journal := editing.FileJournal{Path: filepath.Join(root, "CloudFileAgent2", "editing", req.SessionID, "journal.json")}
	if existing, err := journal.Load(); err == nil {
		if !editing.ValidCheckoutLease(existing.Lease) || existing.Closed {
			return errors.New("existing Checkout journal must be reconciled")
		}
		return json.NewEncoder(output).Encode(map[string]any{"session_id": req.SessionID, "state": "initialized", "existing": true})
	} else if !os.IsNotExist(err) {
		return errors.New("invalid Checkout journal requires explicit recovery")
	}
	// A positive matching device signature is the only way to bootstrap a
	// trusted origin already confirmed by OS Keychain / Windows CNG. Never
	// trust the URI or caller-supplied origin by itself.
	signer, err := installedEditSigner(req.Instance, req.DeviceID, req.SessionID)
	if err != nil {
		return err
	}
	broker, err := editing.NewDeviceCommitBroker(&http.Client{}, req.Instance, req.SessionID, req.DeviceID, signer)
	if err != nil {
		return err
	}
	handoff, err := editing.NewDeviceLeaseBroker(broker)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	lease, err := handoff.Receive(ctx)
	if err != nil {
		return errors.New("current signed Checkout handoff unavailable")
	}
	// This origin is now bound to both the locally enrolled device signing key
	// and the active server-issued lease; do not require a second manual pairing.
	if _, err = config.AddOrigin(req.Instance); err != nil {
		return err
	}
	if _, err = editing.InitializePrivateCheckout(journal, lease); err != nil {
		return err
	}
	// Confirmation erases the server-side encrypted escrow. A lost ACK cannot
	// invalidate the already durable journal; server escrow expires after 10 min.
	acknowledged := handoff.Confirm(ctx) == nil
	return json.NewEncoder(output).Encode(map[string]any{
		"session_id": req.SessionID, "state": "initialized", "existing": false,
		"acknowledged": acknowledged})
}
