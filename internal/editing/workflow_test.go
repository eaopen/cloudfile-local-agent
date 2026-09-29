package editing

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type memoryJournal struct {
	state State
	fail  bool
}

func (j *memoryJournal) Save(s State) error {
	if j.fail {
		return errors.New("disk full")
	}
	j.state = s
	return nil
}

type transport struct {
	intent                     Intent
	prepareCalls, publishCalls int
	lose                       bool
	mismatch                   bool
}

func (r *transport) Prepare(_ context.Context, _ Lease, i Intent) error {
	r.intent = i
	r.prepareCalls++
	return nil
}
func (r *transport) receipt() Receipt {
	id := r.intent.ID
	if r.mismatch {
		id = "other"
	}
	return Receipt{IntentID: id, State: "published", FileID: strings.Repeat("c", 40), ContentDigest: r.intent.Digest, ExpectedFileID: r.intent.ExpectedFileID, Action: r.intent.Action, CheckedIn: r.intent.Action != "commit"}
}
func (r *transport) Publish(_ context.Context, _ Lease, _ Intent) (Receipt, error) {
	r.publishCalls++
	if r.lose {
		return Receipt{}, errors.New("response lost")
	}
	return r.receipt(), nil
}
func (r *transport) Query(_ context.Context, _ Intent) (Receipt, error) { return r.receipt(), nil }
func initial() State {
	return State{Lease: Lease{GuardID: "guard", Generation: "1", CredentialEpoch: "1", BaseFileID: strings.Repeat("b", 40), Token: "private-proof"}}
}
func TestCommitKeepsCheckoutAndCheckinCloses(t *testing.T) {
	r := &transport{}
	w, _ := New(initial(), r, &memoryJournal{})
	if err := w.Commit(context.Background(), "snapshot", strings.Repeat("e", 64), false); err != nil {
		t.Fatal(err)
	}
	if w.State().Closed || w.State().Lease.BaseFileID != strings.Repeat("c", 40) {
		t.Fatal("commit released or did not advance base")
	}
	if err := w.Commit(context.Background(), "snapshot2", strings.Repeat("f", 64), true); err != nil {
		t.Fatal(err)
	}
	if !w.State().Closed {
		t.Fatal("checkin did not close")
	}
}
func TestLostResponseSurvivesRestartAndQueriesOriginalIntent(t *testing.T) {
	j := FileJournal{Path: filepath.Join(t.TempDir(), "edit.json")}
	r := &transport{lose: true}
	w, _ := New(initial(), r, j)
	if err := w.Commit(context.Background(), "snapshot", strings.Repeat("e", 64), false); err == nil {
		t.Fatal("expected uncertain result")
	}
	if err := w.Commit(context.Background(), "new snapshot", strings.Repeat("f", 64), false); !errors.Is(err, ErrPending) {
		t.Fatal("new upload bypassed pending intent")
	}
	saved, err := j.Load()
	if err != nil {
		t.Fatal(err)
	}
	resumed, _ := New(saved, r, j)
	if err := resumed.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resumed.State().Pending != nil || r.publishCalls != 1 {
		t.Fatal("duplicate publication after receipt recovery")
	}
}
func TestJournalFailurePreventsNetworkAndWrongReceiptKeepsIntent(t *testing.T) {
	r := &transport{}
	w, _ := New(initial(), r, &memoryJournal{fail: true})
	if w.Commit(context.Background(), "snapshot", strings.Repeat("e", 64), false) == nil || r.prepareCalls != 0 {
		t.Fatal("upload before durable intent")
	}
	r.mismatch = true
	w, _ = New(initial(), r, &memoryJournal{})
	if w.Commit(context.Background(), "snapshot", strings.Repeat("e", 64), false) == nil || w.State().Pending == nil {
		t.Fatal("wrong receipt accepted")
	}
}

type lifecycleRemote struct {
	transport
	current                 Guard
	resumes, abandons       int
	loseResume, loseAbandon bool
}

func (r *lifecycleRemote) Status(context.Context) (Guard, error) { return r.current, nil }
func (r *lifecycleRemote) Resume(_ context.Context, lease Lease) (Lease, error) {
	if lease.CredentialEpoch != r.current.CredentialEpoch {
		return Lease{}, errors.New("old epoch")
	}
	r.resumes++
	epoch, _ := strconv.Atoi(r.current.CredentialEpoch)
	r.current.CredentialEpoch = strconv.Itoa(epoch + 1)
	r.current.Token = "new-secret"
	if r.loseResume {
		r.loseResume = false
		return Lease{}, errors.New("lost resume reply")
	}
	return r.current.Lease, nil
}
func (r *lifecycleRemote) Heartbeat(context.Context, Lease) error { return nil }
func (r *lifecycleRemote) Abandon(context.Context, Lease) error {
	r.abandons++
	r.current.Active = false
	if r.loseAbandon {
		r.loseAbandon = false
		return errors.New("lost abandon reply")
	}
	return nil
}
func TestLifecycleUnknownResponsesRecoverWithoutReleasingAnotherGeneration(t *testing.T) {
	remote := &lifecycleRemote{current: Guard{Lease: initial().Lease, Active: true, Mode: "checkout"}, loseResume: true, loseAbandon: true}
	workflow, _ := New(initial(), remote, &memoryJournal{})
	if err := workflow.Resume(context.Background()); err == nil {
		t.Fatal("lost credential rotation reported success")
	}
	if err := workflow.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workflow.State().Lease.Generation != "1" || workflow.State().Lease.CredentialEpoch != "3" {
		t.Fatal("resume reused stale epoch or changed occupation generation")
	}
	if err := workflow.Abandon(context.Background()); err == nil {
		t.Fatal("lost release reply reported success")
	}
	if err := workflow.Abandon(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !workflow.State().Closed || workflow.State().Lease.Token != "" || remote.abandons != 1 {
		t.Fatal("release repeated or credentials retained")
	}
	other := &lifecycleRemote{current: Guard{Lease: Lease{GuardID: "another", Generation: "2"}, Active: true, Mode: "checkout"}}
	workflow, _ = New(initial(), other, &memoryJournal{})
	if err := workflow.Abandon(context.Background()); err == nil || other.abandons != 0 {
		t.Fatal("released a newer reservation")
	}
}
func TestPendingIntentBlocksResumeAndAbandonLocally(t *testing.T) {
	state := initial()
	state.Pending = &Intent{ID: "pending"}
	remote := &lifecycleRemote{}
	workflow, _ := New(state, remote, &memoryJournal{})
	if !errors.Is(workflow.Resume(context.Background()), ErrPending) || !errors.Is(workflow.Abandon(context.Background()), ErrPending) {
		t.Fatal("pending local intent bypassed")
	}
	if remote.resumes != 0 || remote.abandons != 0 {
		t.Fatal("contacted release endpoint with unresolved local intent")
	}
}

type cancelRemote struct {
	transport
	state       string
	cancelCalls int
}

func (r *cancelRemote) Query(_ context.Context, i Intent) (Receipt, error) {
	return Receipt{IntentID: i.ID, State: r.state}, nil
}
func (r *cancelRemote) CancelPrepared(_ context.Context, _ Lease, i Intent) (Receipt, error) {
	r.cancelCalls++
	r.state = "cancelled"
	return Receipt{IntentID: i.ID, State: r.state}, nil
}
func TestExplicitCancelRequiresKnownPreparedIntent(t *testing.T) {
	state := initial()
	state.Pending = &Intent{ID: "original", Snapshot: "retained"}
	remote := &cancelRemote{state: "not-found"}
	workflow, _ := New(state, remote, &memoryJournal{})
	if !errors.Is(workflow.CancelPrepared(context.Background()), ErrPending) || workflow.State().Pending == nil || remote.cancelCalls != 0 {
		t.Fatal("absence treated as definitive cancellation")
	}
	remote.state = "prepared"
	if err := workflow.CancelPrepared(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workflow.State().Pending != nil || workflow.State().Closed || remote.cancelCalls != 1 {
		t.Fatal("cancellation did not retain active checkout")
	}
}
