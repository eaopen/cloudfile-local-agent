package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/childproc"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
	"github.com/eaopen/cloudfile-local-agent/internal/fsaccess"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

const leaseInterval = 4 * time.Minute

type leaseEvent struct {
	TaskID   string `json:"task_id"`
	FileName string `json:"file_name"`
	Kind     string `json:"kind"`
}

// The v0.7 journal, not the v0.8 task index, decides whether this original
// Checkout is still active. Never disclose the journal's Lease token.
func activeJournal(sessionID string) (bool, error) {
	if !device.ID.MatchString(sessionID) {
		return false, errors.New("invalid session")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return false, err
	}
	journal := editing.FileJournal{Path: filepath.Join(dir, "CloudFileAgent2", "editing", sessionID, "journal.json")}
	info, err := os.Lstat(journal.Path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !fsaccess.Private(info) {
		return false, errors.New("private journal required")
	}
	state, err := journal.Load()
	if err != nil {
		return false, err
	}
	if state.Closed {
		return false, nil
	}
	if !editing.ValidCheckoutLease(state.Lease) {
		return false, errors.New("original Checkout proof unavailable")
	}
	return true, nil
}

type renewal struct {
	store      *taskcenter.Store
	core       string
	configured bool
	renew      func(context.Context, string, taskcenter.VerifiedSession) error
	last       map[string]string
}

func newRenewal(store *taskcenter.Store, core string) (*renewal, error) {
	if store == nil {
		return nil, errors.New("task index required")
	}
	r := &renewal{store: store, core: core, last: make(map[string]string)}
	if core == "" {
		return r, nil
	}
	if !filepath.IsAbs(core) {
		return nil, errors.New("absolute Go core path required")
	}
	stat, err := os.Lstat(core)
	if err != nil || !stat.Mode().IsRegular() || stat.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("installed Go core executable required")
	}
	r.configured = true
	r.renew = signedChildRenew
	return r, nil
}

func signedChildRenew(ctx context.Context, core string, s taskcenter.VerifiedSession) error {
	payload, err := json.Marshal(map[string]any{
		"instance": s.Instance, "device_id": s.DeviceID, "session_id": s.SessionID,
		"user_confirmed": true,
	})
	if err != nil {
		return err
	}
	childCtx, cancel := context.WithTimeout(ctx, 65*time.Second)
	defer cancel()
	p := exec.CommandContext(childCtx, core, "--renew-checkout-stdin")
	childproc.Hide(p)
	p.Stdin = bytes.NewReader(payload)
	// The signed renewal response is tiny. It must never be logged or sent
	// through Chrome. A failed call is NOT permission to release Checkout.
	var result bytes.Buffer
	p.Stdout = &boundedOutput{b: &result, max: 4096}
	p.Stderr = io.Discard
	if err := p.Run(); err != nil {
		return errors.New("paired Checkout renewal not confirmed")
	}
	var response struct {
		SessionID string `json:"session_id"`
		Renewed   bool   `json:"renewed"`
	}
	if result.Len() > 4096 || json.Unmarshal(result.Bytes(), &response) != nil ||
		response.SessionID != s.SessionID || !response.Renewed {
		return errors.New("signed Checkout renewal receipt missing")
	}
	return nil
}

// RunOneCycle retries no more than once per session per 4-minute cycle.
// It never creates a Checkout, uploads, Checks in, or abandons.
func (r *renewal) RunOneCycle(ctx context.Context) ([]leaseEvent, error) {
	candidates, err := r.store.ExclusiveSessions()
	if err != nil {
		return nil, err
	}
	cfg, configErr := config.Load()
	events := make([]leaseEvent, 0)
	active := make(map[string]bool, len(candidates))
	for _, s := range candidates {
		taskID := sessionKey(s)
		active[taskID] = true
		kind := "checkout-unmanaged"
		if ok, journalErr := activeJournal(s.SessionID); journalErr != nil {
			kind = "checkout-needs-review"
		} else if !ok {
			kind = "checkout-ended"
		} else if configErr != nil || !containsOrigin(cfg.AllowedOrigins, s.Instance) {
			kind = "checkout-needs-review"
		} else if !r.configured || os.Getenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED") != "1" {
			kind = "checkout-unmanaged"
		} else if r.renew == nil || r.renew(ctx, r.core, s) != nil {
			kind = "checkout-renewal-unconfirmed"
		} else {
			kind = "checkout-renewed"
		}
		// Emit changes and actionable failures only once until their state
		// changes. Keep warnings visible in the task list/status later.
		if r.last[taskID] != kind {
			previous := r.last[taskID]
			if (kind != "checkout-renewed" && kind != "checkout-ended") || previous != "" {
				events = append(events, leaseEvent{TaskID: taskID,
					FileName: filepath.Base(s.WorkingCopy), Kind: kind})
			}
			r.last[taskID] = kind
		}
	}
	for id := range r.last {
		if !active[id] {
			delete(r.last, id)
		}
	}
	return events, nil
}
func containsOrigin(origins []string, origin string) bool {
	for _, allowed := range origins {
		if allowed == origin {
			return true
		}
	}
	return false
}
func sessionKey(s taskcenter.VerifiedSession) string {
	// Deterministic task ID is owned by TaskCenter, not guessed here.
	// Resolve by its signed session identity from the local index.
	return taskcenter.SessionTaskID(s)
}

type boundedOutput struct {
	b   *bytes.Buffer
	max int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > w.max {
		return 0, errors.New("unbounded renewal result")
	}
	return w.b.Write(p)
}

// exclusiveCounts is advisory for exit warnings. An unreadable Journal cannot
// be counted as an inactive Checkout; it remains visibly uncertain.
func exclusiveCounts(store *taskcenter.Store) (active, uncertain int, err error) {
	list, e := store.ExclusiveSessions()
	if e != nil {
		return 0, 0, e
	}
	for _, session := range list {
		open, e := activeJournal(session.SessionID)
		if e != nil {
			uncertain++
		} else if open {
			active++
		}
	}
	return active, uncertain, nil
}
