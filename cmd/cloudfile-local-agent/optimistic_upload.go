package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

// Only the installed local controller may nominate this path and approve a
// single upload. Never include this command in Chrome Native Messaging IPC.
type optimisticUploadRequest struct {
	Instance      string `json:"instance"`
	SessionID     string `json:"session_id"`
	DeviceID      string `json:"device_id"`
	BaseFileID    string `json:"base_file_id"`
	WorkingCopy   string `json:"working_copy,omitempty"`
	Action        string `json:"action"`
	UserConfirmed bool   `json:"user_confirmed"`
}
type optimisticSaved struct {
	Instance  string         `json:"instance"`
	SessionID string         `json:"session_id"`
	DeviceID  string         `json:"device_id"`
	Intent    editing.Intent `json:"intent"`
}

// Persistent, owner-only evidence. No server success is inferred from the
// existence of this file; a crash/timeout leaves a PENDING result until the
// user reconciles the original cloud version, without unsafe auto retries.
func writeOptimisticEvidence(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 16384 {
		return errors.New("bounded optimistic upload evidence required")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func optimisticIntentID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func runOptimisticUpload(input io.Reader, output io.Writer) error {
	if !editing.OptimisticUploadEnabled() {
		return errors.New("ordinary Agent upload is disabled")
	}
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) > componentBudget {
		return errors.New("bounded local user action required")
	}
	var req optimisticUploadRequest
	if strictComponentJSON(data, &req) != nil || device.Origin(req.Instance) != nil ||
		!device.ID.MatchString(req.SessionID) || !device.ID.MatchString(req.DeviceID) ||
		len(req.BaseFileID) != 40 || !req.UserConfirmed ||
		(req.Action != "upload" && req.Action != "inspect" && req.Action != "recover") {
		return errors.New("confirmed optimistic local edit required")
	}
	if req.Action == "upload" && !filepath.IsAbs(req.WorkingCopy) {
		return errors.New("approved absolute working file required")
	}
	if (req.Action == "inspect" || req.Action == "recover") && req.WorkingCopy != "" {
		return errors.New("inspect may not read a nominated local working file")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	directory := filepath.Join(root, "CloudFileAgent2", "optimistic", req.SessionID)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted private session directory required")
	}
	pending := filepath.Join(directory, "original-intent.json")
	published := filepath.Join(directory, "published.json")
	if info, statErr := os.Lstat(pending); statErr == nil {
		// A local receipt must never be reusable across paired origins,
		// devices or file baselines. Unknown results are inspected, NOT resent.
		if !info.Mode().IsRegular() || info.Size() > 16384 {
			return errors.New("invalid original optimistic upload journal")
		}
		raw, readErr := os.ReadFile(pending)
		if readErr != nil {
			return errors.New("original optimistic upload evidence unreadable")
		}
		var saved optimisticSaved
		if strictComponentJSON(raw, &saved) != nil ||
			saved.Instance != req.Instance || saved.DeviceID != req.DeviceID ||
			saved.SessionID != req.SessionID || !device.ID.MatchString(saved.Intent.ID) ||
			saved.Intent.ExpectedFileID != req.BaseFileID ||
			saved.Intent.Action != "optimistic-update" {
			return errors.New("optimistic upload origin, device or original baseline changed")
		}
		result := map[string]any{"state": "pending", "session_id": req.SessionID,
			"intent_id": saved.Intent.ID}
		if receiptData, readErr := os.ReadFile(published); readErr == nil {
			var receipt editing.OptimisticResult
			if json.Unmarshal(receiptData, &receipt) == nil &&
				receipt.State == "published" && receipt.IntentID == saved.Intent.ID &&
				receipt.BaseFileID == saved.Intent.ExpectedFileID {
				result = map[string]any{"state": "published", "session_id": req.SessionID, "result": receipt}
			}
		}
		if req.Action == "recover" && result["state"] == "pending" {
			uploader, brokerErr := makeOptimisticUploader(req)
			if brokerErr != nil {
				return errors.New("paired device cannot query original publication")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			receipt, queryErr := uploader.Query(ctx, saved.Intent)
			if queryErr != nil {
				return errors.New("original optimistic status unknown; retain snapshot")
			}
			if receipt.State == "published" {
				if saveErr := writeOptimisticEvidence(published, receipt); saveErr != nil {
					return errors.New("native result confirmed but local receipt storage failed")
				}
				return json.NewEncoder(output).Encode(map[string]any{
					"state": "published", "session_id": req.SessionID, "result": receipt})
			}
			return json.NewEncoder(output).Encode(map[string]any{
				"state": "pending", "intent_id": saved.Intent.ID, "remote_state": receipt.State})
		}
		return json.NewEncoder(output).Encode(result)
	} else if !os.IsNotExist(statErr) {
		return errors.New("existing pending upload must be inspected")
	}
	if req.Action == "inspect" || req.Action == "recover" {
		return json.NewEncoder(output).Encode(map[string]any{"state": "not-found", "session_id": req.SessionID})
	}
	snapshot, err := editing.CaptureSnapshot(req.WorkingCopy, directory, 128*1024*1024)
	if err != nil {
		return err
	}
	intentID, err := optimisticIntentID()
	if err != nil {
		return err
	}
	intent := editing.Intent{ID: intentID, Action: "optimistic-update",
		Snapshot: snapshot.Path, Digest: snapshot.Digest, SnapshotSize: snapshot.Size,
		ExpectedFileID: req.BaseFileID, SourceIdentity: snapshot.SourceIdentity,
		SnapshotCreatedAt: snapshot.CreatedAt}
	if err = writeOptimisticEvidence(pending, optimisticSaved{
		Instance: req.Instance, SessionID: req.SessionID, DeviceID: req.DeviceID, Intent: intent}); err != nil {
		return errors.New("cannot preserve one-shot optimistic upload evidence")
	}
	// The paired key and live session are resolved by the same trusted device
	// signing bridge as exclusive edit. A browser-supplied upload path has no
	// command route here and no Cookie/Ticket fallback is allowed.
	uploader, err := makeOptimisticUploader(req)
	if err != nil {
		return json.NewEncoder(output).Encode(map[string]any{"state": "pending", "intent_id": intent.ID})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	receipt, err := uploader.Upload(ctx, intent)
	if err != nil {
		_ = json.NewEncoder(output).Encode(map[string]any{
			"state": "pending", "intent_id": intent.ID, "recovery": "inspect_remote_first"})
		return errors.New("ordinary device upload outcome unconfirmed; keep local snapshot")
	}
	if err = writeOptimisticEvidence(published, receipt); err != nil {
		return errors.New("native published upload confirmed but local evidence save failed")
	}
	return json.NewEncoder(output).Encode(map[string]any{"state": "published",
		"intent_id": intent.ID, "result": receipt})
}

func makeOptimisticUploader(req optimisticUploadRequest) (*editing.OptimisticUploader, error) {
	signer, err := installedEditSigner(req.Instance, req.DeviceID, req.SessionID)
	if err != nil {
		return nil, err
	}
	broker, err := editing.NewDeviceCommitBroker(&http.Client{}, req.Instance,
		req.SessionID, req.DeviceID, signer)
	if err != nil {
		return nil, err
	}
	return editing.NewOptimisticUploader(broker)
}
