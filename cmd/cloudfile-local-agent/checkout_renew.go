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

// A live user-approved editor may refresh its native Checkout lease without
// uploading, changing files, releasing locks or creating a new Intent.
func runCheckoutRenew(input io.Reader, output io.Writer) error {
	if os.Getenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED") != "1" {
		return errors.New("exclusive device renewal is disabled")
	}
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) > componentBudget {
		return errors.New("bounded renewal request required")
	}
	var req checkoutInitRequest
	if strictComponentJSON(data, &req) != nil || device.Origin(req.Instance) != nil ||
		!device.ID.MatchString(req.SessionID) || !device.ID.MatchString(req.DeviceID) ||
		!req.UserConfirmed {
		return errors.New("current paired device editor required")
	}
	cfg, err := config.Load()
	if err != nil {
		return errors.New("trusted editor configuration unavailable")
	}
	trusted := false
	for _, allowed := range cfg.AllowedOrigins {
		if allowed == req.Instance {
			trusted = true
		}
	}
	if !trusted {
		return errors.New("editing origin is not locally trusted")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	journal := editing.FileJournal{Path: filepath.Join(dir, "CloudFileAgent2", "editing", req.SessionID, "journal.json")}
	state, err := journal.Load()
	if err != nil || state.Closed || !editing.ValidCheckoutLease(state.Lease) {
		return errors.New("current local Checkout journal required")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := handoff.Renew(ctx); err != nil {
		return errors.New("current guarded Checkout renewal failed")
	}
	return json.NewEncoder(output).Encode(map[string]any{"session_id": req.SessionID, "renewed": true})
}
