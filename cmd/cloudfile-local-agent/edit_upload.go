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

// Local controller/CLI input only. It is deliberately absent from the browser
// Native Messaging allowlist: remote pages cannot nominate local upload paths.
type editUploadRequest struct {
	Instance      string `json:"instance"`
	DeviceID      string `json:"device_id"`
	SessionID     string `json:"session_id"`
	WorkingCopy   string `json:"working_copy,omitempty"`
	Action        string `json:"action"`
	UserConfirmed bool   `json:"user_confirmed"`
	IntentID      string `json:"intent_id,omitempty"`
}

func runEditUpload(input io.Reader, output io.Writer) error {
	if os.Getenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED") != "1" {
		return errors.New("exclusive editing is disabled")
	}
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) > componentBudget {
		return errors.New("bounded local edit request required")
	}
	var req editUploadRequest
	if strictComponentJSON(data, &req) != nil || !device.ID.MatchString(req.SessionID) || !device.ID.MatchString(req.DeviceID) || device.Origin(req.Instance) != nil || !req.UserConfirmed {
		return errors.New("explicit local editing request required")
	}
	if req.Action != "commit" && req.Action != "checkin" &&
		req.Action != "recover" && req.Action != "abandon" {
		return errors.New("unsupported manual operation")
	}
	if req.Action == "recover" {
		if req.WorkingCopy != "" || !device.ID.MatchString(req.IntentID) {
			return errors.New("original recovery intent required")
		}
	} else if req.Action == "abandon" {
		if req.WorkingCopy != "" || req.IntentID != "" {
			return errors.New("Checkout abandonment cannot nominate file content")
		}
	} else if !filepath.IsAbs(req.WorkingCopy) || req.IntentID != "" {
		return errors.New("absolute working copy required")
	}
	cfg, err := config.Load()
	if err != nil {
		return errors.New("trusted origin unavailable")
	}
	trusted := false
	for _, origin := range cfg.AllowedOrigins {
		if origin == req.Instance {
			trusted = true
		}
	}
	if !trusted {
		return errors.New("editing origin is not locally trusted")
	}
	signer, err := installedEditSigner(req.Instance, req.DeviceID, req.SessionID)
	if err != nil {
		return err
	}
	broker, err := editing.NewDeviceCommitBroker(&http.Client{}, req.Instance, req.SessionID, req.DeviceID, signer)
	if err != nil {
		return err
	}
	publisher, err := editing.NewDeviceHTTPPublisher(broker)
	if err != nil {
		return err
	}
	transport, err := editing.NewDeviceBoundTransport(broker, publisher)
	if err != nil {
		return err
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	directory := filepath.Join(root, "CloudFileAgent2", "editing", req.SessionID)
	worker, err := editing.OpenSessionWorker(editing.FileJournal{Path: filepath.Join(directory, "journal.json")}, transport)
	if err != nil {
		return errors.New("existing private Checkout journal required")
	}
	defer worker.Close()
	// This is an independent CLI process, not a Native Messaging connection.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if req.Action == "abandon" {
		state := worker.Workflow.State()
		if state.Pending != nil || state.Closed {
			return errors.New("resolve pending Intent before releasing Checkout")
		}
		if err := broker.AbandonCheckout(ctx, state.Lease); err != nil {
			return errors.New("Checkout release unconfirmed; retain private Journal")
		}
		state.Closed = true
		state.Lease.Token = ""
		if err := (editing.FileJournal{Path: filepath.Join(directory, "journal.json")}).Save(state); err != nil {
			return errors.New("Checkout released; local Journal update unconfirmed")
		}
		return json.NewEncoder(output).Encode(map[string]any{
			"session_id": req.SessionID, "state": "abandoned", "released": true})
	}
	if req.Action == "recover" {
		state := worker.Workflow.State()
		if state.Pending != nil && state.Pending.ID == req.IntentID {
			err = worker.Workflow.Recover(ctx)
		} else if state.LastReceipt == nil || state.LastReceipt.IntentID != req.IntentID {
			return errors.New("recovery intent is not in this session")
		}
	} else {
		submitter, createErr := editing.NewManualSubmitter(worker.Workflow)
		if createErr != nil {
			return createErr
		}
		_, err = submitter.Submit(ctx, editing.SubmitRequest{Trigger: editing.SubmitManual, UserConfirmed: true,
			WorkingCopy: req.WorkingCopy, PrivateSessionDir: directory, MaxBytes: 128 * 1024 * 1024, FinishEditing: req.Action == "checkin"})
	}
	state := worker.Workflow.State()
	if err != nil {
		// Pending identity survives even if authorization or the final reply is lost.
		if state.Pending != nil {
			_ = json.NewEncoder(output).Encode(map[string]any{"state": "pending", "intent_id": state.Pending.ID})
		}
		return errors.New("publication unconfirmed; recover the original intent")
	}
	if state.LastReceipt == nil || state.LastReceipt.State != "published" {
		return errors.New("published receipt unavailable")
	}
	return json.NewEncoder(output).Encode(map[string]any{"intent": state.LastReceipt})
}
