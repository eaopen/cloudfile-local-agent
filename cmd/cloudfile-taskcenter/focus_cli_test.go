package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestResidentCLIFocusWithRealTask(t *testing.T) {
	root := t.TempDir()
	working := filepath.Join(root, "presentation.pptx")
	if err := os.WriteFile(working, []byte("open"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Open(taskcenter.OpenRequest{
		Instance: "http://127.0.0.1:8080",
		RepoID:   "11111111-1111-4111-8111-111111111111",
		Path:     "/presentation.pptx", DeviceID: "device", SessionID: "session",
		Mode: "local-edit", BaseFileID: strings.Repeat("a", 40), WorkingCopy: working,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	poll := func() string {
		t.Helper()
		output.Reset()
		if err := execute(context.Background(), []string{"--poll-focus", "--workspace-root", root}, &output); err != nil {
			t.Fatal(err)
		}
		var result struct{ ID string }
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		// Map field is "task_id"; decode explicitly below instead.
		var fields map[string]string
		if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		return fields["task_id"]
	}
	if first := poll(); first != "" {
		t.Fatalf("unexpected initial focus %q", first)
	}
	if err := store.RequestFocus(record.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := poll(); got != record.ID {
		t.Fatalf("focus lost %q", got)
	}
	if again := poll(); again != "" {
		t.Fatalf("focus duplicated %q", again)
	}
}
