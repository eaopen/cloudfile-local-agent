package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestPrivateManualTaskResumesOriginalVerifiedSessionOnly(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	if err := config.Save(config.Config{
		AllowedOrigins: []string{"http://10.12.1.157:8092"}, WorkspaceRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(root, "one.xlsx")
	if err := os.WriteFile(working, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	expected := taskcenter.VerifiedSession{
		Instance:  "http://10.12.1.157:8092",
		DeviceID:  "11111111-1111-4111-8111-111111111111",
		SessionID: "22222222-2222-4222-8222-222222222222",
		Mode:      "local-edit", BaseFileID: strings.Repeat("a", 40), WorkingCopy: working,
	}
	id, _, err := store.RegisterVerifiedSession(expected)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte("{\"task_id\":\"" + id + "\"}")
	var out bytes.Buffer
	if err := runInspectLocalTask(bytes.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var task localTaskInfo
	if err := json.Unmarshal(out.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.WorkingCopy != expected.WorkingCopy || task.BaseFileID != expected.BaseFileID ||
		task.SessionID != expected.SessionID || task.Mode != "local-edit" {
		t.Fatalf("resumed wrong current original session %+v", task)
	}
	if strings.Contains(out.String(), "lease_token") || strings.Contains(out.String(), "intent_id") {
		t.Fatal("private inspect leaked cloud write credentials")
	}
	for _, bad := range [][]byte{
		[]byte(`{"task_id":"../etc/passwd"}`),
		[]byte(`{"task_id":"` + id + `","working_copy":"/etc/passwd"}`),
		[]byte(`{"task_id":"` + id + `","task_id":"` + id + `"}`),
	} {
		if _, err := inspectLocalTask(bytes.NewReader(bad)); err == nil {
			t.Fatalf("untrusted local task source accepted %s", bad)
		}
	}
	// No side effects after a paired origin is withdrawn.
	if err := config.Save(config.Config{AllowedOrigins: []string{"https://10.12.1.157:8092"}, WorkspaceRoot: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLocalTask(bytes.NewReader(request)); err == nil {
		t.Fatal("HTTP task launched under a different HTTPS origin")
	}
}
