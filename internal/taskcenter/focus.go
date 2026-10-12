package taskcenter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const focusTTL = 12 * time.Second

type focusNotice struct {
	ID string `json:"task_id"`
	At int64  `json:"at"`
}

// UI heartbeat is advisory, not proof of a current server session.
// A Chrome click cannot claim success when no resident desktop UI is running.
func (s *Store) ResidentOnline(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online(now)
}
func (s *Store) online(now time.Time) bool {
	info, err := os.Lstat(filepath.Join(s.dir, "resident-heartbeat"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	age := now.Sub(info.ModTime())
	return age > -time.Second && age < focusTTL
}
func (s *Store) focusedTaskExists(id string) bool {
	if _, err := s.read(id); err == nil {
		return true
	}
	_, err := s.readVerifiedSession(id)
	return err == nil
}
func (s *Store) RequestFocus(id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.focusedTaskExists(id) {
		return ErrNotFound
	}
	if !s.online(now) {
		return errors.New("resident TaskCenter not available")
	}
	payload, err := json.Marshal(focusNotice{ID: id, At: now.Unix()})
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".focus-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(payload)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), filepath.Join(s.dir, "focus-request.json"))
}

// PollFocus is called by the ONLY GUI instance while it is running.
// No OS privileges, local network port, browser filesystem permission,
// or second Native Host. Consumed commands expire quickly after crashes.
func (s *Store) PollFocus(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.WriteFile(filepath.Join(s.dir, "resident-heartbeat"), []byte{}, 0600); err != nil {
		return "", err
	}
	file := filepath.Join(s.dir, "focus-request.json")
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1024 {
		return "", ErrCorrupt
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var f focusNotice
	if json.Unmarshal(data, &f) != nil {
		return "", ErrCorrupt
	}
	if err = os.Remove(file); err != nil {
		return "", err
	}
	if now.Unix() < f.At || now.Sub(time.Unix(f.At, 0)) > focusTTL ||
		!s.focusedTaskExists(f.ID) {
		return "", nil
	}
	return f.ID, nil
}
