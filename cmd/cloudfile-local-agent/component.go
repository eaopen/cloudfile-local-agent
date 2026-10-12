package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const componentBudget = 16 * 1024
const componentProtocol = 1

// One request per child. This channel never owns a long-lived upload worker.
func serveComponent(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, componentBudget+1))
	if err != nil || len(data) == 0 || len(data) > componentBudget {
		return fmt.Errorf("invalid component budget")
	}
	var envelope struct {
		IPCVersion int             `json:"ipc_version"`
		Request    json.RawMessage `json:"request"`
	}
	var response any
	code := "invalid_request"
	if err = strictComponentJSON(data, &envelope); err == nil {
		if envelope.IPCVersion != componentProtocol {
			code = "incompatible_protocol"
		} else {
			response, err = componentResult(envelope.Request)
		}
	}
	result := map[string]any{"ipc_version": componentProtocol, "ok": err == nil && response != nil}
	if err != nil || response == nil {
		result["error_code"] = code
	} else {
		result["result"] = response
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > componentBudget {
		return fmt.Errorf("component response exceeded budget")
	}
	_, err = output.Write(encoded)
	return err
}

// Reject duplicates at every object level and trailing data, then enforce the
// typed shape. Request fields cannot override envelope protocol negotiation.
func strictComponentJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var scan func() error
	scan = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			if delim != '{' && delim != '[' {
				return fmt.Errorf("invalid delimiter")
			}
			fields := map[string]bool{}
			for decoder.More() {
				if delim == '{' {
					key, err := decoder.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || fields[name] {
						return fmt.Errorf("duplicate field")
					}
					fields[name] = true
				}
				if err := scan(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := scan(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing input")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func componentResult(data []byte) (any, error) {
	// Reject duplicate fields before decoding; the controller cannot accidentally
	// authorize a different operation from the one the component executes.
	scan := json.NewDecoder(bytes.NewReader(data))
	token, err := scan.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid component object")
	}
	fields := map[string]bool{}
	for scan.More() {
		token, err = scan.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok || fields[name] {
			return nil, fmt.Errorf("duplicate component field")
		}
		fields[name] = true
		var value json.RawMessage
		if err := scan.Decode(&value); err != nil {
			return nil, err
		}
	}
	request := struct {
		nativehost.Request
		SessionID string `json:"session_id"`
		IntentID  string `json:"intent_id"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, fmt.Errorf("trailing component input")
	}
	allowed := map[string][]string{
		"capabilities": {"type"}, "status": {"type"}, "task_summary": {"type"},
		"editing_status":     {"type", "session_id", "intent_id"},
		"open_session":       {"type", "protocol", "server", "ticket", "expires_at", "local_action"},
		"query_local_file":   {"type", "repo_id", "path", "mode"},
		"query_local_folder": {"type", "repo_id", "mode"},
		"open_workspace":     {"type", "repo_id", "path", "mode"},
	}
	names, ok := allowed[request.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported component operation")
	}
	for field := range fields {
		found := false
		for _, name := range names {
			if name == field {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unexpected component field")
		}
	}
	var response any
	if request.Type == "capabilities" {
		response = map[string]any{
			"family": "agent2", "core_version": version, "platform": runtime.GOOS,
			"ipc_version": componentProtocol, "journal_version": 1,
			"manual_submit_core": true, "device_signed_prepare": true,
			"native_device_publish": false, "automatic_upload": false,
		}
	} else if request.Type == "editing_status" {
		canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
		if !canonical.MatchString(request.SessionID) || !canonical.MatchString(request.IntentID) {
			return nil, fmt.Errorf("invalid editing identity")
		}
		directory, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		journal := editing.FileJournal{Path: filepath.Join(directory, "CloudFileAgent2", "editing", request.SessionID, "journal.json")}
		response, err = editing.ReadIntentStatus(journal, request.IntentID)
		if err != nil {
			return nil, err
		}
	} else if request.Type == "task_summary" {
		if !request.Valid() {
			return nil, fmt.Errorf("invalid task summary request")
		}
		cfg, err := config.Load()
		if err != nil || len(cfg.AllowedOrigins) == 0 {
			return nil, fmt.Errorf("trusted local component required")
		}
		root, err := cfg.Root()
		if err != nil {
			return nil, err
		}
		store, err := taskcenter.New(root)
		if err != nil {
			return nil, err
		}
		summaries, err := store.List()
		if err != nil {
			return nil, err
		}
		out := make([]nativehost.TaskSummary, 0, len(summaries))
		for _, t := range summaries {
			if len(out) >= 50 {
				break
			}
			out = append(out, nativehost.TaskSummary{ID: t.ID, FileName: t.FileName,
				Mode: t.Mode, Changed: t.LocalChanged, NeedsReview: t.NeedsReview})
		}
		response = nativehost.Response{OK: true, Tasks: out}
	} else if !request.Valid() {
		return nil, fmt.Errorf("invalid component request")
	} else {
		cfg, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("component config unavailable")
		}
		operations := advertisedCapabilities(cfg.AllowedOrigins)
		// Go core self-update targets a different Host and must never escape the
		// controller. Windows updates replace the entire bundle via its installer.
		capabilities := []string{}
		for _, op := range operations {
			if op != "check_update" && op != "open_session_file" {
				capabilities = append(capabilities, op)
			}
		}
		if request.Type == "status" {
			if len(cfg.AllowedOrigins) > 0 {
				if root, err := cfg.Root(); err == nil {
					if store, err := taskcenter.New(root); err == nil {
						if _, err := store.List(); err == nil {
							capabilities = append(capabilities, "task_summary")
						}
					}
				}
			}
			status := nativehost.Response{OK: true, Version: version, AgentFamily: "agent2", Platform: runtime.GOOS, Capabilities: capabilities, Applications: appfinder.Names()}
			if len(cfg.AllowedOrigins) > 0 {
				if root, err := workspace.Resolve(cfg); err == nil {
					status.WorkspaceRoot = root
					status.CanOpenWorkspace = true
				}
			}
			response = status
		} else {
			enabled := false
			for _, op := range capabilities {
				if op == request.Type {
					enabled = true
				}
			}
			if !enabled {
				return nil, fmt.Errorf("component operation not configured")
			}
			response = handleNativeMessage(request.Request)
		}
	}
	return response, nil
}
