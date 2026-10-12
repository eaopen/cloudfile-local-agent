package taskcenter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifiedSessionTaskWithoutInventedRepo(t *testing.T) {
	root := t.TempDir()
	copy := filepath.Join(root, "memo.docx")
	if err := os.WriteFile(copy, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	v := VerifiedSession{
		Instance:    "https://cloud.example.test",
		DeviceID:    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		SessionID:   "ffffffff-1111-2222-3333-444444444444",
		Mode:        "local-edit",
		BaseFileID:  strings.Repeat("a", 40),
		WorkingCopy: copy,
	}
	id, exists, err := s.RegisterVerifiedSession(v)
	if err != nil || exists || !strings.HasPrefix(id, "session-") {
		t.Fatalf("register %s %v %v", id, exists, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].RepoID != "" || list[0].Path != "" ||
		list[0].FileName != "memo.docx" || list[0].ID != id {
		t.Fatalf("session without fake repo/path: %+v %v", list, err)
	}
	again, exists, err := s.RegisterVerifiedSession(v)
	if err != nil || !exists || again != id {
		t.Fatalf("idempotency: %s %v %v", again, exists, err)
	}
	v.BaseFileID = strings.Repeat("b", 40)
	if _, _, err := s.RegisterVerifiedSession(v); err == nil {
		t.Fatal("reused signed session with another baseline")
	}
	v.BaseFileID = strings.Repeat("a", 40)
	restarted, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	mon, err := NewMonitor(restarted, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := mon.Scan(now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy, []byte("edited twice"), 0600); err != nil {
		t.Fatal(err)
	}
	events, err := mon.Scan(now.Add(time.Second))
	if err != nil || len(events) != 0 {
		t.Fatalf("premature notice %+v %v", events, err)
	}
	events, err = mon.Scan(now.Add(3 * time.Second))
	if err != nil || len(events) != 1 || events[0].TaskID != id || events[0].Kind != "local-saved" {
		t.Fatalf("notice %+v %v", events, err)
	}
}

func TestVerifiedSessionTaskRejectsMalformedAndDamaged(t *testing.T) {
	s, r := fixture(t)
	v := VerifiedSession{
		Instance: "https://cloud.example.test", DeviceID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		SessionID: "ffffffff-1111-2222-3333-444444444444",
		Mode:      "exclusive-edit", BaseFileID: strings.Repeat("c", 40), WorkingCopy: r.WorkingCopy,
	}
	for _, change := range []func(*VerifiedSession){
		func(x *VerifiedSession) { x.Instance = "https://fake.test/path" },
		func(x *VerifiedSession) { x.DeviceID = "spoof" },
		func(x *VerifiedSession) { x.SessionID = "" },
		func(x *VerifiedSession) { x.Mode = "auto-upload" },
		func(x *VerifiedSession) { x.BaseFileID = "unknown" },
	} {
		bad := v
		change(&bad)
		if _, _, err := s.RegisterVerifiedSession(bad); err == nil {
			t.Fatal("accepted invalid proof metadata")
		}
	}
	id, _, err := s.RegisterVerifiedSession(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.filename(id), []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing recovery error: %v", err)
	}
}
