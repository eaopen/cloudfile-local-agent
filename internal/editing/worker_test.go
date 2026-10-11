package editing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerExcludesConcurrentProcessAndPreservesReceipt(t *testing.T) {
	journal := FileJournal{Path: filepath.Join(t.TempDir(), "journal.json")}
	if err := journal.Save(initial()); err != nil {
		t.Fatal(err)
	}
	remote := &transport{lose: true}
	worker, err := OpenSessionWorker(journal, remote)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := OpenSessionWorker(journal, remote); err == nil {
		duplicate.Close()
		t.Fatal("concurrent worker accepted")
	}
	if err = worker.Workflow.Commit(context.Background(), "snapshot", strings.Repeat("e", 64), false); err == nil {
		t.Fatal("lost response reported success")
	}
	intent := worker.Workflow.State().Pending.ID
	status, err := ReadIntentStatus(journal, intent)
	if err != nil || status.State != "pending" {
		t.Fatalf("pending status: %+v %v", status, err)
	}
	worker.Close()
	resumed, err := OpenSessionWorker(journal, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err = resumed.Workflow.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.publishCalls != 1 {
		t.Fatal("recovery repeated upload")
	}
	status, err = ReadIntentStatus(journal, intent)
	if err != nil || status.State != "published" || status.CheckedIn {
		t.Fatalf("published receipt lost: %+v %v", status, err)
	}
	bytes, _ := json.Marshal(status)
	if strings.Contains(string(bytes), "private-proof") || strings.Contains(string(bytes), "snapshot") {
		t.Fatal("status leaked credentials or file paths")
	}
	unknown, err := ReadIntentStatus(journal, "unknown")
	if err != nil || unknown.State != "unknown" || remote.publishCalls != 1 {
		t.Fatal("unknown intent triggered work")
	}
	state, err := journal.Load()
	if err != nil || state.SchemaVersion != 1 || state.LastReceipt == nil {
		t.Fatal("durable receipt/schema missing")
	}
}
func TestWorkerRejectsUnknownJournalSchemaAndMissingPublisher(t *testing.T) {
	journal := FileJournal{Path: filepath.Join(t.TempDir(), "journal.json")}
	if err := os.WriteFile(journal.Path, []byte(`{"schema_version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionWorker(journal, &transport{}); err == nil {
		t.Fatal("unknown schema accepted")
	}
	if _, err := OpenSessionWorker(journal, nil); err == nil {
		t.Fatal("missing publisher accepted")
	}
	if err := journal.Save(initial()); err != nil {
		t.Fatal(err)
	}
	worker, err := OpenSessionWorker(journal, &transport{})
	if err != nil {
		t.Fatal("rejected open leaked process lock", err)
	}
	worker.Close()
}
