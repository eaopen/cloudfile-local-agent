package taskcenter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreferencesDefaultPersistAndTaskListing(t *testing.T) {
	s, r := fixture(t)
	_, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Preferences()
	if err != nil || p.Reminder != "light" || p.FloatPosition != nil {
		t.Fatalf("default light reminder: %+v %v", p, err)
	}
	p.FloatPosition = &FloatPosition{Display: "screen-1", X: 640, Y: 100}
	p.Reminder = "badge"
	if err := s.SavePreferences(p); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(filepath.Dir(s.dir))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Preferences()
	if err != nil || loaded.Reminder != "badge" || loaded.FloatPosition == nil ||
		loaded.FloatPosition.X != 640 {
		t.Fatalf("reload: %+v %v", loaded, err)
	}
	list, err := restarted.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("preferences file confused task scanner: %+v %v", list, err)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 {
		t.Fatal("preferences unexpectedly large")
	}
}

func TestPreferencesValidationAndCorruption(t *testing.T) {
	s, _ := fixture(t)
	for _, p := range []Preferences{
		{Reminder: "auto-upload"},
		{Reminder: "light", FloatPosition: &FloatPosition{Display: "screen", X: 999999}},
	} {
		if err := s.SavePreferences(p); err == nil {
			t.Fatalf("accepted invalid preference %+v", p)
		}
	}
	if err := os.WriteFile(filepath.Join(s.dir, "preferences.json"), []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preferences(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected corrupt prefs, got %v", err)
	}
}
