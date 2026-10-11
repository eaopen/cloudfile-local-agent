package editing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// SubmitTrigger distinguishes the user's explicit save action from future
// unattended save detection. Only explicit manual submissions are supported.
// Neither a local file change nor this value can grant server authority.
type SubmitTrigger string

const SubmitManual SubmitTrigger = "manual"

// SubmitRequest is supplied by a trusted UI session, never by a bare remote
// URI or an unchecked Chrome Native Messaging message. Consent covers exactly
// this single file and one Commit / final Checkin, not background uploads.
type SubmitRequest struct {
	Trigger           SubmitTrigger
	UserConfirmed     bool
	WorkingCopy       string
	PrivateSessionDir string
	MaxBytes          int64
	FinishEditing     bool
}

type SubmitResult struct {
	NewBaseFileID string
	CheckedIn     bool
	SnapshotPath  string
}

// ManualSubmitter is a trigger-independent upload executor. Future opt-in
// auto-submission may call a separately authorized adapter, but cannot quietly
// bypass this version's explicit user-confirmation gate.
//
// It requires a server-authenticated Workflow constructed from the actual
// checkout proof and durable FileJournal. It deliberately does not obtain
// browser cookies, grant Seafile upload tokens, or fall back to update-api.
type ManualSubmitter struct {
	workflow *Workflow
}

func NewManualSubmitter(workflow *Workflow) (*ManualSubmitter, error) {
	if workflow == nil {
		return nil, errors.New("authenticated editing workflow required")
	}
	return &ManualSubmitter{workflow: workflow}, nil
}

func (s *ManualSubmitter) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if req.Trigger != SubmitManual || !req.UserConfirmed {
		return SubmitResult{}, errors.New("an explicit manual commit approval is required")
	}
	if err := ctx.Err(); err != nil {
		return SubmitResult{}, err
	}
	if req.WorkingCopy == "" || req.PrivateSessionDir == "" || req.MaxBytes <= 0 || req.MaxBytes > maxSnapshotBytes {
		return SubmitResult{}, errors.New("bounded working copy and private snapshot directory required")
	}
	original, err := filepath.Abs(req.WorkingCopy)
	if err != nil {
		return SubmitResult{}, err
	}
	private, err := filepath.Abs(req.PrivateSessionDir)
	if err != nil {
		return SubmitResult{}, err
	}
	if original == private {
		return SubmitResult{}, errors.New("working file must not be the session directory")
	}
	dir, err := os.Lstat(private)
	if err != nil {
		return SubmitResult{}, err
	}
	if !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return SubmitResult{}, errors.New("private session must be an existing real directory")
	}
	state := s.workflow.State()
	if state.Closed || state.Lease.GuardID == "" || state.Lease.Token == "" {
		return SubmitResult{}, errors.New("active checkout is required")
	}
	if state.Pending != nil {
		return SubmitResult{}, ErrPending
	}

	captured, err := CaptureSnapshot(original, private, req.MaxBytes)
	if err != nil {
		return SubmitResult{}, err
	}
	// The saved snapshot is intentionally retained for unknown results, even
	// if Publish or journal write fails; never discard the user's sole evidence.
	result := SubmitResult{SnapshotPath: captured.Path}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := s.workflow.CommitCaptured(ctx, captured, req.FinishEditing); err != nil {
		return result, err
	}
	after := s.workflow.State()
	result.NewBaseFileID = after.Lease.BaseFileID
	result.CheckedIn = after.Closed
	return result, nil
}

// Recover is an explicit user-selected action and reuses the persisted
// intent, never silently creating a fresh upload or releasing the checkout.
func (s *ManualSubmitter) Recover(ctx context.Context) error {
	return s.workflow.Recover(ctx)
}
