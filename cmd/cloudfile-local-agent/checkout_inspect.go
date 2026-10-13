package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

// Local UI only: exposes a pending Intent ID, NEVER the stored lease/token,
// upload snapshot path or credential-bearing JSON. It does no network work.
func runCheckoutInspect(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) > componentBudget {
		return errors.New("bounded local status required")
	}
	var req checkoutInitRequest
	if strictComponentJSON(data, &req) != nil || device.Origin(req.Instance) != nil ||
		!device.ID.MatchString(req.SessionID) || !device.ID.MatchString(req.DeviceID) ||
		!req.UserConfirmed {
		return errors.New("current local user confirmation required")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	journal := editing.FileJournal{Path: filepath.Join(root, "CloudFileAgent2", "editing", req.SessionID, "journal.json")}
	state, err := journal.Load()
	if err != nil {
		return errors.New("local edit Checkout journal unavailable")
	}
	result := map[string]any{"session_id": req.SessionID, "state": "ready", "checked_in": state.Closed}
	if state.Pending != nil {
		result["state"] = "pending"
		result["intent_id"] = state.Pending.ID
	} else if state.LastReceipt != nil && state.LastReceipt.State == "published" {
		result["state"] = "published"
		result["intent_id"] = state.LastReceipt.IntentID
	}
	return json.NewEncoder(output).Encode(result)
}
