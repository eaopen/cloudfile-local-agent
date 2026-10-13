package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

type localTaskCommand struct {
	TaskID string `json:"task_id"`
}
type localTaskInfo struct {
	Instance    string `json:"instance"`
	DeviceID    string `json:"device_id"`
	SessionID   string `json:"session_id"`
	Mode        string `json:"mode"`
	BaseFileID  string `json:"base_file_id"`
	WorkingCopy string `json:"working_copy"`
}

// A PRIVATE same-user window-to-core request. Browser, Native Host,
// component IPC and external webpages do NOT expose this command. It
// contains no lease token or upload Intent, and cannot publish a file.
// Every actual edit action remains subject to explicit user consent and
// fresh signed Server authorization in the original v0.7 transaction engine.
func inspectLocalTask(input io.Reader) (localTaskInfo, error) {
	var request localTaskCommand
	data, err := io.ReadAll(io.LimitReader(input, 1025))
	if err != nil || len(data) == 0 || len(data) > 1024 ||
		strictComponentJSON(data, &request) != nil {
		return localTaskInfo{}, errors.New("one bounded local task identity required")
	}
	cfg, err := config.Load()
	if err != nil || len(cfg.AllowedOrigins) == 0 {
		return localTaskInfo{}, errors.New("trusted user origin required")
	}
	root, err := cfg.Root()
	if err != nil {
		return localTaskInfo{}, err
	}
	store, err := taskcenter.New(root)
	if err != nil {
		return localTaskInfo{}, err
	}
	record, err := store.VerifiedSessionByID(request.TaskID)
	if err != nil {
		return localTaskInfo{}, err
	}
	trusted := false
	for _, allowed := range cfg.AllowedOrigins {
		if allowed == record.Instance {
			trusted = true
		}
	}
	if !trusted {
		return localTaskInfo{}, errors.New("original paired origin no longer trusted")
	}
	info, err := os.Lstat(record.WorkingCopy)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return localTaskInfo{}, errors.New("original local edited copy unavailable")
	}
	return localTaskInfo{Instance: record.Instance, DeviceID: record.DeviceID,
		SessionID: record.SessionID, Mode: record.Mode, BaseFileID: record.BaseFileID,
		WorkingCopy: filepath.Clean(record.WorkingCopy)}, nil
}

func runInspectLocalTask(input io.Reader, output io.Writer) error {
	original, err := inspectLocalTask(input)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	if err := enc.Encode(original); err != nil {
		return err
	}
	// Only same-user desktop process stdout may carry the working-copy path.
	_, err = io.Copy(output, &b)
	return err
}
