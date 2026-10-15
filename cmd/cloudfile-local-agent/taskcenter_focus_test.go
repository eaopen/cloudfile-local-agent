package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestComponentFocusOnlyWhenRealResidentIsOnline(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	if err := config.Save(config.Config{
		AllowedOrigins: []string{"http://10.12.1.157:8092"}, WorkspaceRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "draft.docx")
	if err := os.WriteFile(f, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.RegisterVerifiedSession(taskcenter.VerifiedSession{
		Instance:  "http://10.12.1.157:8092",
		DeviceID:  "11111111-1111-4111-8111-111111111111",
		SessionID: "22222222-2222-4222-8222-222222222222",
		Mode:      "local-edit", BaseFileID: strings.Repeat("a", 40), WorkingCopy: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	focus := []byte("{\"type\":\"focus_task\",\"task_id\":\"" + id + "\"}")
	if _, err := componentResult(focus); err == nil {
		t.Fatal("accepted focus without GUI")
	}
	if _, err := store.PollFocus(time.Now()); err != nil {
		t.Fatal(err)
	}
	statusAny, err := componentResult([]byte("{\"type\":\"status\"}"))
	if err != nil {
		t.Fatal(err)
	}
	status := statusAny.(nativehost.Response)
	if !containsCapabilities(status.Capabilities, "focus_task") {
		t.Fatal("no focus capability for live GUI")
	}
	reply, err := componentResult(focus)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(reply)
	if err != nil || !strings.Contains(string(bytes), "true") {
		t.Fatalf("bad focus ack: %s %v", bytes, err)
	}
	actual, err := store.PollFocus(time.Now())
	if err != nil || actual != id {
		t.Fatalf("focus not delivered %s %v", actual, err)
	}
	if _, err := componentResult([]byte("{\"type\":\"focus_task\",\"task_id\":\"" + id + "\",\"path\":\"/etc/passwd\"}")); err == nil {
		t.Fatal("accepted browser nominated path")
	}
}
