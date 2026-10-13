package taskcenter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFocusRequiresLiveResidentAndRealTrustedTask(t *testing.T) {
	s, r := fixture(t)
	task, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.RequestFocus(task.ID, now); err == nil {
		t.Fatal("accepted focus without live GUI")
	}
	if _, err := s.PollFocus(now); err != nil {
		t.Fatal(err)
	}
	if !s.ResidentOnline(now) {
		t.Fatal("heartbeat was not recorded")
	}
	if err := s.RequestFocus("untrusted-file", now); err == nil {
		t.Fatal("accepted arbitrary file")
	}
	if err := s.RequestFocus(task.ID, now); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("focus corrupted normal task list %+v %v", list, err)
	}
	focused, err := s.PollFocus(now.Add(time.Second))
	if err != nil || focused != task.ID {
		t.Fatalf("lost pending focus %q %v", focused, err)
	}
	focused, err = s.PollFocus(now.Add(2 * time.Second))
	if err != nil || focused != "" {
		t.Fatalf("replayed a focus request %q %v", focused, err)
	}
}
func TestFocusExpiryAndCorruptRecord(t *testing.T) {
	s, r := fixture(t)
	task, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.PollFocus(now); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestFocus(task.ID, now); err != nil {
		t.Fatal(err)
	}
	if id, err := s.PollFocus(now.Add(20 * time.Second)); err != nil || id != "" {
		t.Fatalf("stale focus request replayed %q %v", id, err)
	}
	if s.ResidentOnline(now.Add(20 * time.Second)) {
		t.Fatal("stale GUI reported online")
	}
	// An untrusted or torn file is not silently accepted.
	if err := os.WriteFile(filepath.Join(s.dir, "focus-request.json"), []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PollFocus(now); err == nil {
		t.Fatal("corrupt focus queue accepted")
	}
}
