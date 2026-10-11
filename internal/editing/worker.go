package editing

import (
	"errors"
	"os"
	"path/filepath"
)

// SessionWorker holds process-wide ownership through the whole Go workflow.
// A local CLI worker opens this before reading the journal. Chrome/stdio
// timeout must never be its context; product publication remains gated.
type SessionWorker struct {
	Workflow *Workflow
	lock     *os.File
	activity *os.File
}

func OpenSessionWorker(journal FileJournal, transport Transport) (*SessionWorker, error) {
	if transport == nil {
		return nil, errors.New("native device publisher unavailable")
	}
	info, err := os.Lstat(filepath.Dir(journal.Path))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private session directory required")
	}
	activity, err := holdWindowsActivity()
	if err != nil {
		return nil, err
	}
	lock, err := lockSession(filepath.Join(filepath.Dir(journal.Path), ".operation.lock"))
	if err != nil {
		if activity != nil {
			activity.Close()
		}
		return nil, err
	}
	worker := &SessionWorker{lock: lock, activity: activity}
	state, err := journal.Load()
	if err == nil {
		worker.Workflow, err = New(state, transport, journal)
	}
	if err != nil {
		worker.Close()
		return nil, err
	}
	return worker, nil
}
func (w *SessionWorker) Close() error {
	if w.activity != nil {
		defer w.activity.Close()
	}
	return w.lock.Close()
}

// IntentStatus is deliberately credential/path-free. A local pending intent is
// never a success receipt, and unknown IDs never create or retry an operation.
type IntentStatus struct {
	IntentID  string `json:"intent_id"`
	State     string `json:"state"`
	CheckedIn bool   `json:"checked_in"`
	FileID    string `json:"result_file_id,omitempty"`
}

func ReadIntentStatus(journal FileJournal, intentID string) (IntentStatus, error) {
	state, err := journal.Load()
	if err != nil {
		return IntentStatus{}, err
	}
	if state.Pending != nil && state.Pending.ID == intentID {
		return IntentStatus{IntentID: intentID, State: "pending"}, nil
	}
	if r := state.LastReceipt; r != nil && r.IntentID == intentID && r.State == "published" {
		return IntentStatus{IntentID: intentID, State: "published", CheckedIn: r.CheckedIn, FileID: r.FileID}, nil
	}
	return IntentStatus{IntentID: intentID, State: "unknown"}, nil
}
