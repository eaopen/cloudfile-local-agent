package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func TestPreferenceCLIPreservesReminderAndPosition(t *testing.T) {
	root := t.TempDir()
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePreferences(taskcenter.Preferences{Reminder: "badge"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"--set-position", "--x", "120", "--y", "-35", "--display", "main", "--workspace-root", root}
	if err := execute(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := execute(context.Background(), []string{"--get-preferences", "--workspace-root", root}, &out); err != nil {
		t.Fatal(err)
	}
	var p taskcenter.Preferences
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Reminder != "badge" || p.FloatPosition == nil ||
		p.FloatPosition.X != 120 || p.FloatPosition.Y != -35 || p.FloatPosition.Display != "main" {
		t.Fatalf("lost previously selected reminder or position: %+v", p)
	}
	for _, invalid := range [][]string{
		{"--set-position", "--x", "5", "--workspace-root", root},
		{"--get-preferences", "--x", "1", "--workspace-root", root},
		{"--list", "--get-preferences", "--workspace-root", root},
		{"--set-position", "--x", "999999", "--y", "0", "--workspace-root", root},
	} {
		if err := execute(context.Background(), invalid, &out); err == nil {
			t.Fatalf("accepted invalid taskcenter CLI arguments: %v", invalid)
		}
	}
}
