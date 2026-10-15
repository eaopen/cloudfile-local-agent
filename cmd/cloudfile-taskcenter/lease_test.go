package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

func leaseFixture(t *testing.T) (*renewal, taskcenter.VerifiedSession, string) {
	t.Helper()
	isolateUserConfig(t)
	t.Setenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED", "1")
	root := t.TempDir()
	if err := config.Save(config.Config{AllowedOrigins: []string{"http://10.12.1.157:8092"}, WorkspaceRoot: root}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "drawing.dwg")
	if err := os.WriteFile(file, []byte("initial-drawing"), 0600); err != nil {
		t.Fatal(err)
	}
	session := taskcenter.VerifiedSession{
		Instance:  "http://10.12.1.157:8092",
		DeviceID:  "11111111-1111-4111-8111-111111111111",
		SessionID: "22222222-2222-4222-8222-222222222222",
		Mode:      "exclusive-edit", BaseFileID: strings.Repeat("a", 40), WorkingCopy: file,
	}
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RegisterVerifiedSession(session); err != nil {
		t.Fatal(err)
	}
	userConfig, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	jdir := filepath.Join(userConfig, "CloudFileAgent2", "editing", session.SessionID)
	if err := os.MkdirAll(jdir, 0700); err != nil {
		t.Fatal(err)
	}
	journal := editing.FileJournal{Path: filepath.Join(jdir, "journal.json")}
	state := editing.State{Lease: editing.Lease{
		GuardID:    "33333333-3333-4333-8333-333333333333",
		Generation: "1", CredentialEpoch: "1",
		BaseFileID: strings.Repeat("a", 40), Token: strings.Repeat("c", 64),
	}}
	if err := journal.Save(state); err != nil {
		t.Fatal(err)
	}
	core := filepath.Join(root, "fake-go-core")
	if err := os.WriteFile(core, []byte("original-core"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := newRenewal(store, core)
	if err != nil {
		t.Fatal(err)
	}
	return r, session, journal.Path
}

func TestResidentRenewsOnlyOriginalActiveCheckout(t *testing.T) {
	r, s, journal := leaseFixture(t)
	calls := 0
	r.renew = func(_ context.Context, core string, v taskcenter.VerifiedSession) error {
		calls++
		if v.SessionID != s.SessionID || v.Instance != s.Instance {
			t.Fatal("wrong original signed session")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		events, err := r.RunOneCycle(context.Background())
		if err != nil || len(events) != 0 {
			t.Fatalf("repeated renewal %+v %v", events, err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected independent scheduled cycles, got %d", calls)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var state editing.State
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Closed = true
	if err = (editing.FileJournal{Path: journal}).Save(state); err != nil {
		t.Fatal(err)
	}
	events, err := r.RunOneCycle(context.Background())
	if err != nil || len(events) != 1 || events[0].Kind != "checkout-ended" || calls != 2 {
		t.Fatalf("closed checkout renewed: %+v %v", events, err)
	}
}

func TestResidentWarnsWithoutCoreOrCurrentOrigin(t *testing.T) {
	r, s, _ := leaseFixture(t)
	r.configured = false
	events, err := r.RunOneCycle(context.Background())
	if err != nil || len(events) != 1 || events[0].Kind != "checkout-unmanaged" {
		t.Fatalf("missing meaningful warning: %+v %v", events, err)
	}
	events, err = r.RunOneCycle(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatalf("warning spam: %+v %v", events, err)
	}
	t.Setenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED", "0")
	r.configured = true
	events, err = r.RunOneCycle(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatalf("unexpected duplicate disabled warning %+v %v", events, err)
	}
	_ = s
}

func TestResidentNeverRenewsWithWrongScheme(t *testing.T) {
	r, _, _ := leaseFixture(t)
	if err := config.Save(config.Config{AllowedOrigins: []string{"https://10.12.1.157:8092"}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.renew = func(context.Context, string, taskcenter.VerifiedSession) error { calls++; return nil }
	events, err := r.RunOneCycle(context.Background())
	if err != nil || calls != 0 || len(events) != 1 || events[0].Kind != "checkout-needs-review" {
		t.Fatalf("cross-scheme renewal not blocked: %+v %v", events, err)
	}
}

func TestRenewalClientRequiresOriginalLocalBinary(t *testing.T) {
	root := t.TempDir()
	store, err := taskcenter.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRenewal(store, "/definitely-missing/go-agent"); err == nil {
		t.Fatal("allowed absent signing core")
	}
	if _, err := newRenewal(store, "relative/go-agent"); err == nil {
		t.Fatal("allowed relative signing core")
	}
	r, err := newRenewal(store, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, err := r.RunOneCycle(ctx)
	if err != nil || len(events) != 0 {
		t.Fatalf("empty runtime error: %+v %v", events, err)
	}
}

func TestExclusiveCloseWarningFailsSafe(t *testing.T) {
	r, session, journal := leaseFixture(t)
	active, uncertain, err := exclusiveCounts(r.store)
	if err != nil || active != 1 || uncertain != 0 {
		t.Fatalf("active count %d %d %v", active, uncertain, err)
	}
	// A destroyed private journal is uncertainty, not proof that Checkout
	// was released or that it is safe to quit.
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	active, uncertain, err = exclusiveCounts(r.store)
	if err != nil || active != 0 || uncertain != 1 {
		t.Fatalf("unknown count %d %d %v", active, uncertain, err)
	}
	if session.Mode != "exclusive-edit" {
		t.Fatal("invalid fixture")
	}
}
