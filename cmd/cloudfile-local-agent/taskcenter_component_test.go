package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestWindowsComponentTaskSummaryReturnsOnlySafeLocalMetadata(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	if err := config.Save(config.Config{
		AllowedOrigins: []string{"https://files.example.test"},
		WorkspaceRoot:  root,
	}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "doc.xlsx")
	if err := os.WriteFile(file, []byte("approved current document"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.RegisterVerifiedSession(taskcenter.VerifiedSession{
		Instance:  "https://files.example.test",
		DeviceID:  "11111111-1111-4111-8111-111111111111",
		SessionID: "22222222-2222-4222-8222-222222222222",
		Mode:      "local-edit", BaseFileID: strings.Repeat("a", 40),
		WorkingCopy: file,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := componentResult([]byte(`{"type":"task_summary"}`))
	if err != nil {
		t.Fatal(err)
	}
	view, ok := response.(nativehost.Response)
	if !ok || !view.OK || len(view.Tasks) != 1 || view.Tasks[0].ID != id {
		t.Fatalf("unexpected task component response: %+v", response)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{file, "working_copy", "device_id", "session_id", "base_file_id", "token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("browser-visible task contains %s", secret)
		}
	}
	statusAny, err := componentResult([]byte(`{"type":"status"}`))
	if err != nil {
		t.Fatal(err)
	}
	status, ok := statusAny.(nativehost.Response)
	if !ok || !containsCapabilities(status.Capabilities, "task_summary") {
		t.Fatalf("Windows Host capability gating omitted task_summary: %+v", statusAny)
	}
	if _, err := componentResult([]byte(`{"type":"task_summary","path":"/tmp/fake"}`)); err == nil {
		t.Fatal("accepted untrusted arbitrary file path in Chrome request")
	}
}
