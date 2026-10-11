package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestLocalReadOnlyTaskListing(t *testing.T) {
	root := t.TempDir()
	copy := filepath.Join(root, "edited.docx")
	if err := os.WriteFile(copy, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Open(taskcenter.OpenRequest{
		Instance: "https://test.example", RepoID: "01234567-89ab-cdef-0123-456789abcdef",
		Path: "/edited.docx", DeviceID: "device-1", SessionID: "session-1",
		BaseFileID: strings.Repeat("a", 40), WorkingCopy: copy, Mode: "local-edit",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"--list", "--workspace-root", root}, &out); err != nil {
		t.Fatal(err)
	}
	var parsed listResult
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.SchemaVersion != 1 || len(parsed.Tasks) != 1 ||
		parsed.Tasks[0].FileName != "edited.docx" {
		t.Fatalf("unexpected summaries: %+v", parsed)
	}
	for _, bad := range []string{copy, "session-1", "device-1", "token", "working_copy"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("sensitive field leaked: %s", bad)
		}
	}
}

func TestNoUploadOrWatchSideEffects(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if err := execute(ctx, []string{"--watch", "--workspace-root", root}, &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("unexpected watcher side effect: %q", output.String())
	}
	for _, args := range [][]string{
		{"--workspace-root", root},
		{"--list", "--watch", "--workspace-root", root},
		{"--list", "--workspace-root", root, "unexpected"},
	} {
		if err := execute(ctx, args, &output); err == nil {
			t.Fatalf("accepted invalid action %v", args)
		}
	}
}
