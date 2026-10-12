package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestWindowsLocalControllerIndexDoesNotGrantCloudAccess(t *testing.T) {
	root := t.TempDir()
	copy := filepath.Join(root, "current.docx")
	if err := os.WriteFile(copy, []byte("current local document"), 0600); err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{
		"instance":   "https://files.example.test",
		"device_id":  "11111111-1111-4111-8111-111111111111",
		"session_id": "22222222-2222-4222-8222-222222222222",
		"mode":       "optimistic-edit", "base_file_id": strings.Repeat("a", 40),
		"working_copy": copy,
	}
	payload, _ := json.Marshal(fields)
	var out bytes.Buffer
	if err := indexLocalSessionInRoot(bytes.NewReader(payload), &out, root); err != nil {
		t.Fatal(err)
	}
	var result struct {
		State  string `json:"state"`
		ID     string `json:"task_id"`
		Reused bool   `json:"reused"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "indexed" || result.Reused || !strings.HasPrefix(result.ID, "session-") {
		t.Fatalf("%+v", result)
	}
	if strings.Contains(out.String(), copy) || strings.Contains(out.String(), "device_id") {
		t.Fatal("Go subprocess response leaked local path or proof metadata")
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 1 || tasks[0].RepoID != "" || tasks[0].Mode != "local-edit" {
		t.Fatalf("wrong task from original verified session: %+v %v", tasks, err)
	}
	out.Reset()
	if err := indexLocalSessionInRoot(bytes.NewReader(payload), &out, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"reused":true`) {
		t.Fatalf("repeat recreated same task: %s", out.String())
	}
	fields["base_file_id"] = strings.Repeat("b", 40)
	conflict, _ := json.Marshal(fields)
	if err := indexLocalSessionInRoot(bytes.NewReader(conflict), &out, root); err == nil {
		t.Fatal("accepted a replacement baseline for an existing task")
	}
	fields["base_file_id"] = strings.Repeat("a", 40)
	fields["untrusted_ticket"] = "secret"
	unknown, _ := json.Marshal(fields)
	if err := indexLocalSessionInRoot(bytes.NewReader(unknown), &out, root); err == nil {
		t.Fatal("accepted unexpected remote credential in the local task record")
	}
}
