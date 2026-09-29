// Package editing coordinates manual single-file commits. It is not registered
// in the legacy v2 runner: a verified server transport is required before use.
package editing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
)

var ErrPending = errors.New("resolve the pending edit intent first")

type Lease struct {
	GuardID         string `json:"guard_id"`
	Generation      string `json:"generation"`
	CredentialEpoch string `json:"credential_epoch"`
	BaseFileID      string `json:"base_file_id"`
	// The journal holds this secret with owner-only permissions, never in logs.
	Token string `json:"token"`
}
type Intent struct {
	ID                string `json:"intent_id"`
	Snapshot          string `json:"snapshot"`
	Digest            string `json:"content_digest"`
	ExpectedFileID    string `json:"expected_file_id"`
	Action            string `json:"action"`
	SourceIdentity    string `json:"source_identity,omitempty"`
	SnapshotSize      int64  `json:"snapshot_size,omitempty"`
	SnapshotCreatedAt string `json:"snapshot_created_at,omitempty"`
}
type State struct {
	Lease   Lease   `json:"lease"`
	Pending *Intent `json:"pending,omitempty"`
	Closed  bool    `json:"closed"`
}
type Receipt struct {
	IntentID       string `json:"intent_id"`
	State          string `json:"state"`
	FileID         string `json:"result_file_id"`
	ContentDigest  string `json:"content_digest"`
	ExpectedFileID string `json:"expected_file_id"`
	Action         string `json:"action"`
	CheckedIn      bool   `json:"checked_in"`
}

// Transport authenticates the device, checks the fixed server origin and sends
// exact guard/epoch proof. No ordinary update-api fallback is permitted.
type Transport interface {
	Prepare(context.Context, Lease, Intent) error
	Publish(context.Context, Lease, Intent) (Receipt, error)
	Query(context.Context, Intent) (Receipt, error)
}

// Journal.Save must atomically persist before returning; workflow never uploads
// a snapshot until its intent is durable. The snapshot is retained after errors.
type Journal interface{ Save(State) error }

type Workflow struct {
	mu      sync.Mutex
	state   State
	remote  Transport
	journal Journal
}

func New(state State, remote Transport, journal Journal) (*Workflow, error) {
	if remote == nil || journal == nil || state.Lease.GuardID == "" || (state.Lease.Token == "" && !state.Closed) {
		return nil, errors.New("authenticated checkout and durable journal required")
	}
	if state.Pending != nil {
		pending := *state.Pending
		state.Pending = &pending
	}
	return &Workflow{state: state, remote: remote, journal: journal}, nil
}
func (w *Workflow) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.state
	if s.Pending != nil {
		p := *s.Pending
		s.Pending = &p
	}
	return s
}
func (w *Workflow) save(s State) error {
	if err := w.journal.Save(s); err != nil {
		return err
	}
	w.state = s
	return nil
}

// Commit is invoked only by an explicit user action. checkin performs a final
// content commit and releases remotely; ordinary commits retain the checkout.
func (w *Workflow) Commit(ctx context.Context, snapshot, digest string, checkin bool) error {
	return w.CommitCaptured(ctx, SnapshotDetails{Path: snapshot, Digest: digest}, checkin)
}

// CommitCaptured is only an explicit user action. Capture the working copy with
// CaptureSnapshot first; an application save event never calls this method.
func (w *Workflow) CommitCaptured(ctx context.Context, captured SnapshotDetails, checkin bool) error {
	snapshot, digest := captured.Path, captured.Digest
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Closed {
		return errors.New("checkout is closed")
	}
	if w.state.Pending != nil {
		return ErrPending
	}
	hash, err := hex.DecodeString(digest)
	if err != nil || len(hash) != 32 || snapshot == "" {
		return errors.New("immutable snapshot and SHA256 required")
	}
	intentID, err := newIntentID()
	if err != nil {
		return err
	}
	action := "commit"
	if checkin {
		action = "checkin"
	}
	s := w.state
	s.Pending = &Intent{ID: intentID,
		Snapshot: snapshot, Digest: digest, ExpectedFileID: s.Lease.BaseFileID, Action: action,
		SourceIdentity: captured.SourceIdentity, SnapshotSize: captured.Size, SnapshotCreatedAt: captured.CreatedAt}
	if err := w.save(s); err != nil {
		return err
	}
	if err := w.remote.Prepare(ctx, s.Lease, *s.Pending); err != nil {
		return err
	}
	receipt, err := w.remote.Publish(ctx, s.Lease, *s.Pending)
	if err != nil {
		return err
	}
	return w.accept(receipt)
}

// Recover never creates a new intent or replaces the user's working copy.
func (w *Workflow) Recover(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Pending == nil {
		return nil
	}
	intent := *w.state.Pending
	receipt, err := w.remote.Query(ctx, intent)
	if err != nil {
		return err
	}
	if receipt.IntentID != intent.ID {
		return errors.New("receipt does not match pending intent")
	}
	switch receipt.State {
	case "not-found":
		if err := w.remote.Prepare(ctx, w.state.Lease, intent); err != nil {
			return err
		}
	case "prepared":
	case "published":
		return w.accept(receipt)
	default:
		return errors.New("publication requires user recovery")
	}
	receipt, err = w.remote.Publish(ctx, w.state.Lease, intent)
	if err != nil {
		return err
	}
	return w.accept(receipt)
}
func (w *Workflow) accept(r Receipt) error {
	p := w.state.Pending
	fileID, err := hex.DecodeString(r.FileID)
	if p == nil || r.State != "published" || r.IntentID != p.ID || r.Action != p.Action ||
		(p.Action != "commit" && !r.CheckedIn) || (p.Action == "checkin-unchanged" && r.FileID != p.ExpectedFileID) || r.ContentDigest != p.Digest || r.ExpectedFileID != p.ExpectedFileID || err != nil || len(fileID) != 20 {
		return errors.New("unconfirmed publication receipt")
	}
	s := w.state
	s.Lease.BaseFileID = r.FileID
	s.Closed = p.Action == "checkin" || p.Action == "checkin-unchanged"
	s.Pending = nil
	if s.Closed {
		s.Lease.Token = ""
	}
	return w.save(s)
}

// CheckinUnchanged still creates a durable intent; the server native finalizer
// verifies the real baseline and atomically releases, without uploading bytes.
func (w *Workflow) CheckinUnchanged(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Closed {
		return errors.New("checkout is closed")
	}
	if w.state.Pending != nil {
		return ErrPending
	}
	id, err := newIntentID()
	if err != nil {
		return err
	}
	empty := sha256.Sum256(nil)
	state := w.state
	state.Pending = &Intent{ID: id, Digest: hex.EncodeToString(empty[:]), ExpectedFileID: state.Lease.BaseFileID, Action: "checkin-unchanged"}
	if err = w.save(state); err != nil {
		return err
	}
	if err = w.remote.Prepare(ctx, state.Lease, *state.Pending); err != nil {
		return err
	}
	result, err := w.remote.Publish(ctx, state.Lease, *state.Pending)
	if err != nil {
		return err
	}
	return w.accept(result)
}

// LifecycleTransport keeps explicit user recovery separate from publication.
// Automatic lease timers and device authentication are owned by the host.
type LifecycleTransport interface {
	Status(context.Context) (Guard, error)
	Resume(context.Context, Lease) (Lease, error)
	Heartbeat(context.Context, Lease) error
	Abandon(context.Context, Lease) error
}

func (w *Workflow) Heartbeat(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	remote, ok := w.remote.(LifecycleTransport)
	if !ok || w.state.Closed {
		return errors.New("checkout lifecycle transport unavailable")
	}
	return remote.Heartbeat(ctx, w.state.Lease)
}

// Resume requires a new authenticated host scope. Read current epoch first so
// a lost Resume response or failed journal save can be recovered by an explicit
// fresh Resume, never by resurrecting an old proof.
func (w *Workflow) Resume(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Pending != nil {
		return ErrPending
	}
	remote, ok := w.remote.(LifecycleTransport)
	if !ok || w.state.Closed {
		return errors.New("checkout lifecycle transport unavailable")
	}
	current, err := remote.Status(ctx)
	if err != nil {
		return err
	}
	state := w.state
	if !current.Active || current.Mode != "checkout" || current.GuardID != state.Lease.GuardID ||
		current.Generation != state.Lease.Generation || current.BaseFileID != state.Lease.BaseFileID || current.PendingIntent != nil {
		return errors.New("reservation changed; resolve server state")
	}
	lease := state.Lease
	lease.CredentialEpoch = current.CredentialEpoch
	next, err := remote.Resume(ctx, lease)
	if err != nil {
		return err
	}
	state.Lease = next
	return w.save(state)
}
func (w *Workflow) Abandon(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Pending != nil {
		return ErrPending
	}
	remote, ok := w.remote.(LifecycleTransport)
	if !ok || w.state.Closed {
		return errors.New("checkout lifecycle transport unavailable")
	}
	current, err := remote.Status(ctx)
	if err != nil {
		return err
	}
	// A prior explicit release with a lost response is already complete. A later
	// generation cannot be released using this old reservation's proof.
	if current.Active && (current.GuardID != w.state.Lease.GuardID || current.Generation != w.state.Lease.Generation) {
		return errors.New("reservation changed; resolve server state")
	}
	if current.PendingIntent != nil {
		return ErrPending
	}
	if current.Active {
		if err = remote.Abandon(ctx, w.state.Lease); err != nil {
			return err
		}
	}
	state := w.state
	state.Closed = true
	state.Lease.Token = ""
	return w.save(state)
}

// CancelPrepared is an explicit user recovery action. Absence in one query
// cannot prove an earlier timed-out request will not arrive later; that case
// remains pending. A concurrently published intent is recovered as success.
func (w *Workflow) CancelPrepared(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Pending == nil {
		return nil
	}
	intent := *w.state.Pending
	result, err := w.remote.Query(ctx, intent)
	if err != nil {
		return err
	}
	if result.IntentID != intent.ID {
		return errors.New("receipt does not match pending intent")
	}
	if result.State == "published" {
		return w.accept(result)
	}
	if result.State != "prepared" && result.State != "cancelled" {
		return ErrPending
	}
	if result.State == "prepared" {
		remote, ok := w.remote.(interface {
			CancelPrepared(context.Context, Lease, Intent) (Receipt, error)
		})
		if !ok {
			return errors.New("explicit intent cancellation unavailable")
		}
		result, err = remote.CancelPrepared(ctx, w.state.Lease, intent)
		if err != nil {
			return err
		}
		if result.IntentID != intent.ID || result.State != "cancelled" {
			return errors.New("intent cancellation not confirmed")
		}
	}
	state := w.state
	state.Pending = nil
	return w.save(state)
}
