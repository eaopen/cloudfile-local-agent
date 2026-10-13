package editing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type manualTransport struct {
	intent    Intent
	published int
	lost      bool
}

func (t *manualTransport) Prepare(_ context.Context, _ Lease, i Intent) error {
	t.intent = i
	return nil
}
func (t *manualTransport) Publish(_ context.Context, _ Lease, i Intent) (Receipt, error) {
	t.published++
	if t.lost {
		return Receipt{}, errors.New("result unknown")
	}
	return t.receipt(), nil
}
func (t *manualTransport) receipt() Receipt {
	return Receipt{IntentID: t.intent.ID, State: "published", FileID: strings.Repeat("c", 40), ContentDigest: t.intent.Digest, ExpectedFileID: t.intent.ExpectedFileID, Action: t.intent.Action, CheckedIn: t.intent.Action == "checkin"}
}
func (t *manualTransport) Query(_ context.Context, _ Intent) (Receipt, error) {
	return t.receipt(), nil
}
func TestExplicitManualUploadWithMultipleCommitsAndFinalCheckin(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "drawing.dwg")
	if err := os.WriteFile(work, []byte("revision one"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := &manualTransport{}
	journal := FileJournal{Path: filepath.Join(dir, "journal.json")}
	workflow, err := New(initial(), remote, journal)
	if err != nil {
		t.Fatal(err)
	}
	submitter, err := NewManualSubmitter(workflow)
	if err != nil {
		t.Fatal(err)
	}
	req := SubmitRequest{Trigger: SubmitManual, WorkingCopy: work, PrivateSessionDir: dir, MaxBytes: 1024, UserConfirmed: true}
	for i := 0; i < 2; i++ {
		if i == 1 {
			if err = os.WriteFile(work, []byte("revision two"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		result, err := submitter.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if result.CheckedIn || result.NewBaseFileID != strings.Repeat("c", 40) {
			t.Fatalf("prematurely checked in %+v", result)
		}
		if result.SnapshotPath == "" {
			t.Fatal("missing retained snapshot")
		}
		if _, err = os.Stat(result.SnapshotPath); err != nil {
			t.Fatal(err)
		}
	}
	req.FinishEditing = true
	result, err := submitter.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CheckedIn || remote.published != 3 || workflow.State().Lease.Token != "" {
		t.Fatalf("expected explicit checkin, got %+v published=%d", result, remote.published)
	}
	if content, err := os.ReadFile(work); err != nil || string(content) != "revision two" {
		t.Fatal("uploaded flow changed editable source")
	}
}
func TestManualUploadNeverTriggeredWithoutUserConsent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "edit.docx")
	_ = os.WriteFile(file, []byte("user saved"), 0600)
	remote := &manualTransport{}
	workflow, _ := New(initial(), remote, FileJournal{Path: filepath.Join(dir, "edit.json")})
	submitter, _ := NewManualSubmitter(workflow)
	request := SubmitRequest{Trigger: "auto", WorkingCopy: file, PrivateSessionDir: dir, MaxBytes: 1024, UserConfirmed: true}
	if _, err := submitter.Submit(context.Background(), request); err == nil {
		t.Fatal("automatic trigger must not be allowed")
	}
	request.Trigger = SubmitManual
	request.UserConfirmed = false
	if _, err := submitter.Submit(context.Background(), request); err == nil {
		t.Fatal("unconfirmed submit was allowed")
	}
	if remote.published != 0 {
		t.Fatal("uploaded without manual approval")
	}
	request.UserConfirmed = true
	request.MaxBytes = 3
	if _, err := submitter.Submit(context.Background(), request); err == nil {
		t.Fatal("size limit was not checked")
	}
	if remote.published != 0 {
		t.Fatal("uploaded oversized working copy")
	}
}
func TestManualUploadLostReplyNeedsSameIntentAndPreservesSnapshot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "drawing.step")
	_ = os.WriteFile(file, []byte("submitted content"), 0600)
	remote := &manualTransport{lost: true}
	journal := FileJournal{Path: filepath.Join(dir, "edit.json")}
	workflow, _ := New(initial(), remote, journal)
	submitter, _ := NewManualSubmitter(workflow)
	request := SubmitRequest{Trigger: SubmitManual, WorkingCopy: file, PrivateSessionDir: dir, MaxBytes: 1024, UserConfirmed: true}
	result, err := submitter.Submit(context.Background(), request)
	if err == nil || result.SnapshotPath == "" {
		t.Fatalf("expected retained snapshot on unknown result: %+v %v", result, err)
	}
	state, err := journal.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.Snapshot != result.SnapshotPath {
		t.Fatal("pending intent or snapshot not durable")
	}
	_ = os.WriteFile(file, []byte("new local changes"), 0600)
	if _, err := submitter.Submit(context.Background(), request); !errors.Is(err, ErrPending) {
		t.Fatal("new intent submitted while original unresolved")
	}
	restored, _ := New(state, remote, journal)
	recovery, _ := NewManualSubmitter(restored)
	if err := recovery.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.published != 1 || restored.State().Pending != nil {
		t.Fatal("recovery uploaded again or remained pending")
	}
	if b, _ := os.ReadFile(file); string(b) != "new local changes" {
		t.Fatal("recovery overwrote working copy")
	}
	if b, _ := os.ReadFile(result.SnapshotPath); string(b) != "submitted content" {
		t.Fatal("recovery discarded prior snapshot")
	}
}
