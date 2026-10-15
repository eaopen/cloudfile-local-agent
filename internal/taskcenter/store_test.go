package taskcenter

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testRepo = "01234567-89ab-cdef-0123-456789abcdef"
const testBase = "0123456789abcdef0123456789abcdef01234567"

func fixture(t *testing.T) (*Store, OpenRequest) {
	t.Helper()
	root := t.TempDir()
	working := filepath.Join(root, "work", "report.docx")
	if err := os.MkdirAll(filepath.Dir(working), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(working, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, OpenRequest{
		Instance: "https://cloud.example.test", RepoID: testRepo,
		Path: "/Shared/report.docx", DeviceID: "paired-device",
		SessionID: "session-1", Mode: "local-edit", BaseFileID: testBase,
		WorkingCopy: working,
	}
}

func TestOpenListAndRestart(t *testing.T) {
	s, r := fixture(t)
	first, reused, err := s.Open(r)
	if err != nil || reused {
		t.Fatalf("first open: reused=%v err=%v", reused, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].LocalChanged ||
		list[0].FileName != "report.docx" {
		t.Fatalf("initial summary: %+v %v", list, err)
	}
	if err := os.WriteFile(r.WorkingCopy, []byte("edited content"), 0600); err != nil {
		t.Fatal(err)
	}
	// Browser reconnect provides a different ticket and server baseline.
	r.SessionID = "session-2"
	r.BaseFileID = strings.Repeat("a", 40)
	other, reused, err := s.Open(r)
	if err != nil || !reused || other.ID != first.ID ||
		other.BaseFileID != first.BaseFileID || other.SessionID != first.SessionID {
		t.Fatalf("reopen overwrote original identity: %+v reused=%v err=%v", other, reused, err)
	}
	restarted, err := New(filepath.Dir(s.dir))
	if err != nil {
		t.Fatal(err)
	}
	list, err = restarted.List()
	if err != nil || len(list) != 1 || !list[0].LocalChanged {
		t.Fatalf("restart lost pending local change: %+v err=%v", list, err)
	}
	fromDisk, err := restarted.Get(first.ID)
	if err != nil || fromDisk.WorkingCopy != r.WorkingCopy {
		t.Fatalf("read: %+v %v", fromDisk, err)
	}
	raw, err := os.ReadFile(s.filename(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "intent") {
		t.Fatal("task index must not store edit credentials or intents")
	}
}

func TestReopenCannotChangeEditMode(t *testing.T) {
	s, r := fixture(t)
	original, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Mode = "exclusive-edit"
	existing, reused, err := s.Open(r)
	if !reused || !errors.Is(err, ErrModeConflict) || existing.ID != original.ID || existing.Mode != "local-edit" {
		t.Fatalf("edit mode silently changed: %+v %v %v", existing, reused, err)
	}
}

func TestSameNameInDifferentReposAndMissingLocal(t *testing.T) {
	s, r := fixture(t)
	first, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(filepath.Dir(r.WorkingCopy), "other")
	if err := os.MkdirAll(otherDir, 0700); err != nil {
		t.Fatal(err)
	}
	otherCopy := filepath.Join(otherDir, "report.docx")
	if err := os.WriteFile(otherCopy, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	r.RepoID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	r.WorkingCopy = otherCopy
	second, _, err := s.Open(r)
	if err != nil || first.ID == second.ID {
		t.Fatalf("two repo identities collide: %v", err)
	}
	if err := os.Remove(otherCopy); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	missing := 0
	for _, v := range list {
		if v.LocalMissing && v.NeedsReview {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("missing copy must require manual review: %+v", list)
	}
}

func TestNoOverwriteOrSecretsOnInvalidInput(t *testing.T) {
	s, r := fixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(*OpenRequest)
	}{
		{"traversal", func(q *OpenRequest) { q.Path = "/a/../report.docx" }},
		{"malicious origin", func(q *OpenRequest) { q.Instance = "https://user:pass@example.test" }},
		{"missing baseline", func(q *OpenRequest) { q.BaseFileID = "" }},
		{"invalid mode", func(q *OpenRequest) { q.Mode = "auto-upload" }},
		{"unverified device", func(q *OpenRequest) { q.DeviceID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := r
			tc.mutate(&bad)
			if _, _, err := s.Open(bad); err == nil {
				t.Fatal("accepted malformed edit task")
			}
		})
	}
	if err := os.Symlink(r.WorkingCopy, filepath.Join(filepath.Dir(r.WorkingCopy), "link.docx")); err != nil {
		t.Fatal(err)
	}
	r.WorkingCopy = filepath.Join(filepath.Dir(r.WorkingCopy), "link.docx")
	if _, _, err := s.Open(r); err == nil {
		t.Fatal("accepted symlink as new working copy")
	}
	list, err := s.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("invalid requests persisted: %+v %v", list, err)
	}
}

func TestCorruptAndUnprivateStoreAreVisible(t *testing.T) {
	s, r := fixture(t)
	first, _, err := s.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.filename(first.ID), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(first.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get returned %v", err)
	}
	if _, err := s.List(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("List returned %v", err)
	}
	// The privacy half of this test is POSIX-only. Windows has no permission
	// bits, so os.Chmod cannot make the store "world-readable" and
	// fsaccess.Private deliberately accepts every path there (access is
	// governed by NTFS ACLs, which os.FileInfo does not expose).
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := New(filepath.Dir(s.dir)); err == nil {
			t.Fatal("accepted world-readable task index")
		}
	}
}
