package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

// This command is local-only and callable by the installed paired .NET GUI
// after it has verified the CNG proof and downloaded the real working copy.
// It ONLY indexes the local file; never uploads, renews or releases Checkout.
// Any process with the same OS user's file access can create private metadata,
// so it MUST NOT be treated as proof of server authorization.
func indexLocalSession(input io.Reader, output io.Writer) error {
	rootConfig, err := config.Load()
	if err != nil {
		return errors.New("local workspace configuration unavailable")
	}
	root, err := workspace.Resolve(rootConfig)
	if err != nil {
		return errors.New("local workspace unavailable")
	}
	return indexLocalSessionInRoot(input, output, root)
}

func indexLocalSessionInRoot(input io.Reader, output io.Writer, root string) error {
	var req struct {
		Instance    string `json:"instance"`
		DeviceID    string `json:"device_id"`
		SessionID   string `json:"session_id"`
		Mode        string `json:"mode"`
		BaseFileID  string `json:"base_file_id"`
		WorkingCopy string `json:"working_copy"`
	}
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(data) > 4096 {
		return errors.New("bounded local task record required")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errors.New("invalid local session metadata")
	}
	var leftover any
	if err := dec.Decode(&leftover); err != io.EOF {
		return errors.New("one local task record required")
	}
	if req.Mode == "optimistic-edit" {
		req.Mode = "local-edit"
	}
	store, err := taskcenter.New(root)
	if err != nil {
		return err
	}
	id, reused, err := store.RegisterVerifiedSession(taskcenter.VerifiedSession{
		Instance: req.Instance, DeviceID: req.DeviceID, SessionID: req.SessionID,
		Mode: req.Mode, BaseFileID: req.BaseFileID, WorkingCopy: req.WorkingCopy,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{
		"state": "indexed", "task_id": id, "reused": reused,
	})
}
