// Package taskcenter stores metadata about local edit tasks, never checkout
// secrets or upload intents. The v0.7 editing journal remains authoritative.
package taskcenter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const maxRecordSize = 16 * 1024

var (
	ErrNotFound     = errors.New("task not found")
	ErrModeConflict = errors.New("existing task has a different edit mode")
	ErrCorrupt      = errors.New("invalid task record: manual recovery required")
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	idPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// OpenRequest must originate from a verified local session or Checkout.
// Browser messages alone must never create trusted edit tasks.
type OpenRequest struct {
	Instance    string
	RepoID      string
	Path        string
	DeviceID    string
	SessionID   string
	Mode        string // local-edit or exclusive-edit
	BaseFileID  string
	WorkingCopy string
}

type Task struct {
	SchemaVersion     int    `json:"schema_version"`
	ID                string `json:"task_id"`
	Instance          string `json:"instance"`
	RepoID            string `json:"repo_id"`
	Path              string `json:"path"`
	DeviceID          string `json:"device_id"`
	SessionID         string `json:"session_id"`
	Mode              string `json:"mode"`
	BaseFileID        string `json:"base_file_id"`
	WorkingCopy       string `json:"working_copy"`
	BaselineSize      int64  `json:"baseline_size"`
	BaselineMTimeNano int64  `json:"baseline_mtime_ns"`
	CreatedAt         int64  `json:"created_at"`
}

// Summary omits local paths, device/session IDs, and all credentials.
// LocalChanged is only a hint, not evidence that upload is safe.
type Summary struct {
	ID           string `json:"task_id"`
	FileName     string `json:"file_name"`
	RepoID       string `json:"repo_id"`
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	LocalChanged bool   `json:"local_changed"`
	LocalMissing bool   `json:"local_missing"`
	NeedsReview  bool   `json:"needs_review"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func New(workspaceRoot string) (*Store, error) {
	if !filepath.IsAbs(workspaceRoot) {
		return nil, errors.New("absolute workspace root required")
	}
	dir := filepath.Join(workspaceRoot, ".cloudfile-taskcenter")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private task directory required")
	}
	return &Store{dir: dir}, nil
}

func identity(instance, repoID, remotePath, deviceID string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{instance, repoID, remotePath, deviceID}, "\x00")))
	return hex.EncodeToString(h[:])
}

func validate(r OpenRequest) error {
	u, err := url.Parse(r.Instance)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		r.Instance != u.Scheme+"://"+u.Host {
		return errors.New("canonical instance origin required")
	}
	if !uuidPattern.MatchString(r.RepoID) || workspace.RelPath(r.Path) == "" {
		return errors.New("canonical library identity required")
	}
	if r.DeviceID == "" || len(r.DeviceID) > 128 || r.SessionID == "" || len(r.SessionID) > 128 ||
		strings.ContainsAny(r.DeviceID+r.SessionID, "\x00\r\n") {
		return errors.New("verified device and session identities required")
	}
	if r.Mode != "local-edit" && r.Mode != "exclusive-edit" {
		return errors.New("invalid edit mode")
	}
	b, err := hex.DecodeString(r.BaseFileID)
	if err != nil || len(b) != 20 {
		return errors.New("original file ID required")
	}
	if !filepath.IsAbs(r.WorkingCopy) || strings.ContainsRune(r.WorkingCopy, '\x00') {
		return errors.New("absolute working copy required")
	}
	return nil
}

func request(t Task) OpenRequest {
	return OpenRequest{Instance: t.Instance, RepoID: t.RepoID, Path: t.Path, DeviceID: t.DeviceID,
		SessionID: t.SessionID, Mode: t.Mode, BaseFileID: t.BaseFileID, WorkingCopy: t.WorkingCopy}
}

// Open never overwrites a prior task, even when the browser returns with a
// different session/base file ID. The original local copy must be preserved.
func (s *Store) Open(r OpenRequest) (Task, bool, error) {
	if err := validate(r); err != nil {
		return Task{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := identity(r.Instance, r.RepoID, r.Path, r.DeviceID)
	if t, err := s.read(id); err == nil {
		if t.Mode != r.Mode {
			return t, true, ErrModeConflict
		}
		return t, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Task{}, false, err
	}
	info, err := os.Lstat(r.WorkingCopy)
	if err != nil {
		return Task{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Task{}, false, errors.New("regular working copy required")
	}
	t := Task{SchemaVersion: 1, ID: id, Instance: r.Instance, RepoID: r.RepoID, Path: r.Path,
		DeviceID: r.DeviceID, SessionID: r.SessionID, Mode: r.Mode, BaseFileID: r.BaseFileID,
		WorkingCopy: r.WorkingCopy, BaselineSize: info.Size(),
		BaselineMTimeNano: info.ModTime().UnixNano(), CreatedAt: time.Now().Unix()}
	data, err := json.Marshal(t)
	if err != nil {
		return Task{}, false, err
	}
	f, err := os.CreateTemp(s.dir, ".cf-task-*")
	if err != nil {
		return Task{}, false, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return Task{}, false, err
	}
	if closeErr != nil {
		return Task{}, false, closeErr
	}
	// An atomic hard link publishes only a fully written record and does
	// not replace a record created by another process during this call.
	if err := os.Link(f.Name(), s.filename(id)); err != nil {
		if os.IsExist(err) {
			other, readErr := s.read(id)
			return other, readErr == nil, readErr
		}
		return Task{}, false, err
	}
	return t, false, nil
}

func (s *Store) filename(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) read(id string) (Task, error) {
	if !idPattern.MatchString(id) {
		return Task{}, ErrNotFound
	}
	info, err := os.Lstat(s.filename(id))
	if os.IsNotExist(err) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 ||
		info.Size() <= 0 || info.Size() > maxRecordSize {
		return Task{}, ErrCorrupt
	}
	data, err := os.ReadFile(s.filename(id))
	if err != nil {
		return Task{}, err
	}
	var t Task
	if json.Unmarshal(data, &t) != nil || t.SchemaVersion != 1 || validate(request(t)) != nil ||
		t.ID != id || identity(t.Instance, t.RepoID, t.Path, t.DeviceID) != id {
		return Task{}, ErrCorrupt
	}
	return t, nil
}

func (s *Store) Get(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(id)
}

// List rebuilds task summaries after restarts. A malformed record is reported
// as a recovery error rather than silently hidden. No network side effects.
func (s *Store) List() ([]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if e.Name() == "preferences.json" || e.Name() == "focus-request.json" || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(e.Name(), "session-") {
			record, err := s.readVerifiedSession(strings.TrimSuffix(e.Name(), ".json"))
			if err != nil {
				return nil, fmt.Errorf("session task %s: %w", e.Name(), err)
			}
			out = append(out, summaryVerifiedSession(record))
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, err := s.read(id)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", id, err)
		}
		v := Summary{ID: t.ID, FileName: path.Base(t.Path), RepoID: t.RepoID, Path: t.Path, Mode: t.Mode}
		info, err := os.Lstat(t.WorkingCopy)
		switch {
		case os.IsNotExist(err):
			v.LocalMissing = true
			v.NeedsReview = true
		case err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0:
			v.NeedsReview = true
		case info.Size() != t.BaselineSize || info.ModTime().UnixNano() != t.BaselineMTimeNano:
			v.LocalChanged = true
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
