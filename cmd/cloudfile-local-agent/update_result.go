package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
)

type updateResult struct {
	State string `json:"state"`
	Kind  string `json:"kind"`
	At    int64  `json:"at"`
}

func recordUpdateResult(kind, state string) {
	if runtime.GOOS != "darwin" {
		return
	}
	dir := home.Dir()
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".update-result-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(updateResult{State: state, Kind: kind, At: time.Now().Unix()})
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(file.Name(), filepath.Join(dir, "update-result.json"))
	}
}
func describeUpdateResult(response *nativehost.Response) {
	if runtime.GOOS != "darwin" {
		return
	}
	data, err := os.ReadFile(filepath.Join(home.Dir(), "update-result.json"))
	if err != nil || len(data) > 1024 {
		return
	}
	var result updateResult
	if json.Unmarshal(data, &result) != nil || (result.Kind != "agent" && result.Kind != "extension") ||
		(result.State != "running" && result.State != "completed" && result.State != "failed") {
		return
	}
	response.LastUpdateState = result.State
	response.LastUpdateKind = result.Kind
}
