package taskcenter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
)

// VerifiedSession is created in-process only AFTER signed device claim,
// authorized read and successful creation of the local working copy.
// The v0.7 grant doesn't include the original repo/path; never manufacture it.
// Identity is scoped to the authenticated session rather than the file.
type VerifiedSession struct {
	Instance    string
	DeviceID    string
	SessionID   string
	Mode        string
	BaseFileID  string
	WorkingCopy string
}

type sessionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"task_id"`
	Instance      string `json:"instance"`
	DeviceID      string `json:"device_id"`
	SessionID     string `json:"session_id"`
	Mode          string `json:"mode"`
	BaseFileID    string `json:"base_file_id"`
	WorkingCopy   string `json:"working_copy"`
	Size          int64  `json:"baseline_size"`
	MTimeNano     int64  `json:"baseline_mtime_ns"`
	CreatedAt     int64  `json:"created_at"`
}

var fileID40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validVerified(v VerifiedSession) bool {
	return device.Origin(v.Instance) == nil &&
		device.ID.MatchString(v.DeviceID) && device.ID.MatchString(v.SessionID) &&
		(v.Mode == "local-edit" || v.Mode == "exclusive-edit") &&
		fileID40.MatchString(v.BaseFileID) &&
		filepath.IsAbs(v.WorkingCopy) && !strings.ContainsRune(v.WorkingCopy, 0)
}
func sessionIdentity(v VerifiedSession) string {
	sum := sha256.Sum256([]byte("verified-session\x00" + v.Instance + "\x00" + v.DeviceID + "\x00" + v.SessionID))
	return "session-" + hex.EncodeToString(sum[:])
}
func verifiedRequest(r sessionRecord) VerifiedSession {
	return VerifiedSession{Instance: r.Instance, DeviceID: r.DeviceID, SessionID: r.SessionID,
		Mode: r.Mode, BaseFileID: r.BaseFileID, WorkingCopy: r.WorkingCopy}
}

// RegisterVerifiedSession is a local index operation, never an authorization.
// Caller must have checked the signed device proof and current download grant.
// Existing records never change, even when the same ticket is presented again.
func (s *Store) RegisterVerifiedSession(v VerifiedSession) (string, bool, error) {
	if !validVerified(v) {
		return "", false, errors.New("authenticated local edit session required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := sessionIdentity(v)
	if previous, err := s.readVerifiedSession(id); err == nil {
		if previous.WorkingCopy != v.WorkingCopy || previous.Mode != v.Mode ||
			previous.BaseFileID != v.BaseFileID {
			return "", true, errors.New("verified edit session identity conflict")
		}
		return id, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", false, err
	}
	info, err := os.Lstat(v.WorkingCopy)
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("regular authorized working copy required")
	}
	r := sessionRecord{SchemaVersion: 1, ID: id, Instance: v.Instance, DeviceID: v.DeviceID,
		SessionID: v.SessionID, Mode: v.Mode, BaseFileID: v.BaseFileID,
		WorkingCopy: v.WorkingCopy, Size: info.Size(), MTimeNano: info.ModTime().UnixNano(),
		CreatedAt: time.Now().Unix()}
	bytes, err := json.Marshal(r)
	if err != nil {
		return "", false, err
	}
	temp, err := os.CreateTemp(s.dir, ".cf-session-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(bytes)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return "", false, err
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	if err = os.Link(temp.Name(), s.filename(id)); err != nil {
		if os.IsExist(err) {
			previous, readErr := s.readVerifiedSession(id)
			if readErr != nil {
				return "", false, readErr
			}
			if previous.WorkingCopy != v.WorkingCopy || previous.Mode != v.Mode ||
				previous.BaseFileID != v.BaseFileID {
				return "", true, errors.New("verified edit session identity conflict")
			}
			return id, true, nil
		}
		return "", false, err
	}
	return id, false, nil
}

func (s *Store) readVerifiedSession(id string) (sessionRecord, error) {
	var r sessionRecord
	if !strings.HasPrefix(id, "session-") || !idPattern.MatchString(strings.TrimPrefix(id, "session-")) {
		return r, ErrNotFound
	}
	info, err := os.Lstat(s.filename(id))
	if os.IsNotExist(err) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > maxRecordSize {
		return r, ErrCorrupt
	}
	data, err := os.ReadFile(s.filename(id))
	if err != nil {
		return r, err
	}
	if json.Unmarshal(data, &r) != nil || r.SchemaVersion != 1 || r.ID != id ||
		!validVerified(verifiedRequest(r)) || sessionIdentity(verifiedRequest(r)) != id ||
		r.Size < 0 || r.CreatedAt <= 0 {
		return sessionRecord{}, ErrCorrupt
	}
	return r, nil
}

func summaryVerifiedSession(r sessionRecord) Summary {
	s := Summary{ID: r.ID, FileName: filepath.Base(r.WorkingCopy), Mode: r.Mode}
	info, err := os.Lstat(r.WorkingCopy)
	switch {
	case os.IsNotExist(err):
		s.LocalMissing = true
		s.NeedsReview = true
	case err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0:
		s.NeedsReview = true
	case info.Size() != r.Size || info.ModTime().UnixNano() != r.MTimeNano:
		s.LocalChanged = true
	}
	return s
}

// WorkingCopyForMonitor is internal-only; browser summaries never include it.
func (s *Store) WorkingCopyForMonitor(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(id, "session-") {
		r, err := s.readVerifiedSession(id)
		return r.WorkingCopy, err
	}
	r, err := s.read(id)
	return r.WorkingCopy, err
}

// ExclusiveSessions is for the same-user resident supervisor only. Never
// expose this result through Native Messaging: it contains session metadata.
func (s *Store) ExclusiveSessions() ([]VerifiedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]VerifiedSession, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		rec, err := s.readVerifiedSession(id)
		if err != nil {
			return nil, fmt.Errorf("exclusive task index requires reconciliation: %w", err)
		}
		if rec.Mode == "exclusive-edit" {
			out = append(out, verifiedRequest(rec))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// SessionTaskID returns the stable key for this already-validated session.
// It does not establish device authority.
func SessionTaskID(s VerifiedSession) string { return sessionIdentity(s) }
