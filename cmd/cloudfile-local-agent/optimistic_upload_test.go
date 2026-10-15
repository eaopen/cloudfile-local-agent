package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

func TestOptimisticLocalIntentCanBeInspectedWithoutReupload(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED", "1")
	session := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	intent := "33333333-3333-4333-8333-333333333333"
	original := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	origin := "https://cloudfile.invalid"
	request := optimisticUploadRequest{Instance: origin, SessionID: session,
		DeviceID: device, BaseFileID: original, Action: "inspect", UserConfirmed: true}
	inspect := func(value optimisticUploadRequest) (map[string]any, error) {
		raw, _ := json.Marshal(value)
		output := &bytes.Buffer{}
		err := runOptimisticUpload(bytes.NewReader(raw), output)
		if err != nil {
			return nil, err
		}
		response := map[string]any{}
		err = json.Unmarshal(output.Bytes(), &response)
		return response, err
	}
	first, err := inspect(request)
	if err != nil || first["state"] != "not-found" {
		t.Fatalf("empty local status %+v %v", first, err)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "CloudFileAgent2", "optimistic", session)
	created := optimisticSaved{Instance: origin, SessionID: session, DeviceID: device,
		Intent: editing.Intent{ID: intent, Action: "optimistic-update", ExpectedFileID: original}}
	if err = writeOptimisticEvidence(filepath.Join(folder, "original-intent.json"), created); err != nil {
		t.Fatal(err)
	}
	pending, err := inspect(request)
	if err != nil || pending["state"] != "pending" || pending["intent_id"] != intent {
		t.Fatalf("original intent is not recoverable: %+v %v", pending, err)
	}
	altered := request
	altered.BaseFileID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	next, err := inspect(altered)
	if err != nil || next["state"] != "not-found" {
		t.Fatalf("new baseline must have an independent empty journal: %+v %v", next, err)
	}
	altered = request
	altered.DeviceID = "44444444-4444-4444-8444-444444444444"
	if _, err = inspect(altered); err == nil {
		t.Fatal("foreign device reused local upload intent")
	}
	receipt := editing.OptimisticResult{IntentID: intent, State: "published",
		FileID: "cccccccccccccccccccccccccccccccccccccccc", BaseFileID: original}
	if err = writeOptimisticEvidence(filepath.Join(folder, "published.json"), receipt); err != nil {
		t.Fatal(err)
	}
	saved, err := inspect(request)
	if err != nil || saved["state"] != "published" {
		t.Fatalf("durable native confirmation missing %+v %v", saved, err)
	}
	// A second explicit save is a different base, never a rewrite of the
	// first original Intent or reuse of its published acknowledgement.
	newPending, newPublished := optimisticEvidencePaths(folder,
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if newPending == filepath.Join(folder, "original-intent.json") ||
		newPublished == filepath.Join(folder, "published.json") {
		t.Fatal("continued save reused old immutable journal")
	}
}
