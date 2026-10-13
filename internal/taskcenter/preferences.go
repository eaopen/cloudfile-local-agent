package taskcenter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Preferences are local GUI options, not authorizations. Neither changing the
// reminder nor moving the icon permits uploading or releasing a checkout.
type Preferences struct {
	Reminder      string         `json:"reminder"` // light (default), badge, expand
	FloatPosition *FloatPosition `json:"float_position,omitempty"`
}

type FloatPosition struct {
	Display string `json:"display"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
}

func DefaultPreferences() Preferences { return Preferences{Reminder: "light"} }

func (p Preferences) validate() error {
	if p.Reminder != "light" && p.Reminder != "badge" && p.Reminder != "expand" {
		return errors.New("unknown reminder mode")
	}
	if f := p.FloatPosition; f != nil {
		if len(f.Display) > 128 || f.X < -32768 || f.X > 32768 || f.Y < -32768 || f.Y > 32768 {
			return errors.New("invalid desktop icon position")
		}
	}
	return nil
}

func (s *Store) Preferences() (Preferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := DefaultPreferences()
	file := filepath.Join(s.dir, "preferences.json")
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return p, ErrCorrupt
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(data, &p); err != nil || p.validate() != nil {
		return DefaultPreferences(), ErrCorrupt
	}
	return p, nil
}

func (s *Store) SavePreferences(p Preferences) error {
	if err := p.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".cf-prefs-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	return os.Rename(f.Name(), filepath.Join(s.dir, "preferences.json"))
}
