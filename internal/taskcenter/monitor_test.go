package taskcenter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMonitorStableSaveAndNoAutomaticUpload(t *testing.T) {
	s, r := fixture(t)
	task, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMonitor(s, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	assertEvents := func(now time.Time, want string) {
		t.Helper()
		got, err := m.Scan(now)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			if len(got) != 0 {
				t.Fatalf("unexpected notices: %+v", got)
			}
			return
		}
		if len(got) != 1 || got[0].Kind != want || got[0].TaskID != task.ID {
			t.Fatalf("got %+v want %s", got, want)
		}
	}
	assertEvents(start, "")
	if err := os.WriteFile(r.WorkingCopy, []byte("edited-1"), 0600); err != nil {
		t.Fatal(err)
	}
	assertEvents(start.Add(time.Second), "")
	assertEvents(start.Add(2*time.Second), "")
	assertEvents(start.Add(5*time.Second), "local-saved")
	assertEvents(start.Add(8*time.Second), "")
	assertEvents(start.Add(10*time.Second), "")
	// Auto-save may only update mtime. The same content must not notify again.
	if err := os.Chtimes(r.WorkingCopy, start.Add(time.Minute), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertEvents(start.Add(11*time.Second), "")
	assertEvents(start.Add(15*time.Second), "")
	// Repeated file writes reset quiet period, and only the final save is shown.
	if err := os.WriteFile(r.WorkingCopy, []byte("edited-again"), 0600); err != nil {
		t.Fatal(err)
	}
	assertEvents(start.Add(16*time.Second), "")
	if err := os.WriteFile(r.WorkingCopy, []byte("final-edited-content"), 0600); err != nil {
		t.Fatal(err)
	}
	assertEvents(start.Add(17*time.Second), "")
	assertEvents(start.Add(21*time.Second), "local-saved")
}

func TestMonitorMissingAndSymlinkAreWarningsNotSaved(t *testing.T) {
	s, r := fixture(t)
	_, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMonitor(s, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _ = m.Scan(start)
	if err := os.Remove(r.WorkingCopy); err != nil {
		t.Fatal(err)
	}
	e, err := m.Scan(start.Add(time.Second))
	if err != nil || len(e) != 1 || e[0].Kind != "needs-review" {
		t.Fatalf("missing: %+v %v", e, err)
	}
	e, err = m.Scan(start.Add(2 * time.Second))
	if err != nil || len(e) != 0 {
		t.Fatalf("warning repeated: %+v %v", e, err)
	}
	external := filepath.Join(t.TempDir(), "external.docx")
	if err := os.WriteFile(external, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, r.WorkingCopy); err != nil {
		t.Fatal(err)
	}
	e, err = m.Scan(start.Add(3 * time.Second))
	if err != nil || len(e) != 0 {
		t.Fatalf("followed symlink or rewarned: %+v %v", e, err)
	}
	if err := os.Remove(r.WorkingCopy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.WorkingCopy, []byte("new-real-file"), 0600); err != nil {
		t.Fatal(err)
	}
	e, err = m.Scan(start.Add(4 * time.Second))
	if err != nil || len(e) != 0 {
		t.Fatalf("premature saved notice: %+v %v", e, err)
	}
	e, err = m.Scan(start.Add(6 * time.Second))
	if err != nil || len(e) != 1 || e[0].Kind != "local-saved" {
		t.Fatalf("recovered: %+v %v", e, err)
	}
}

func TestMonitorRestoreStartsWithQuietPeriod(t *testing.T) {
	s, r := fixture(t)
	_, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.WorkingCopy, []byte("new after restart"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(filepath.Dir(s.dir))
	if err != nil {
		t.Fatal(err)
	}
	mon, err := NewMonitor(restarted, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := mon.Scan(start)
	if err != nil || len(got) != 0 {
		t.Fatalf("restart claimed stable save: %+v %v", got, err)
	}
	got, err = mon.Scan(start.Add(4 * time.Second))
	if err != nil || len(got) != 1 || got[0].Kind != "local-saved" {
		t.Fatalf("restart lost change: %+v %v", got, err)
	}
}

func TestMonitorValidatesInput(t *testing.T) {
	if _, err := NewMonitor(nil, 3*time.Second); err == nil {
		t.Fatal("accepted nil store")
	}
	s, _ := fixture(t)
	if _, err := NewMonitor(s, 0); err == nil {
		t.Fatal("accepted zero stabilization")
	}
}
