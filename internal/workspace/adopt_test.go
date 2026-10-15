package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
)

const adoptRepo = "01234567-89ab-cdef-0123-456789abcdef"

// isolateUserConfig points every platform's user-config location at a fresh
// temporary directory.  os.UserConfigDir() resolves differently per platform
// (%AppData% on Windows, $HOME on macOS, $XDG_CONFIG_HOME elsewhere), so all of
// them have to be redirected: the mirror marker lives next to config.json, and
// a test must never touch the developer's real profile.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "appdata", "Local"))
}

func writeMirror(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(data)
}

// A mirror that lives in the platform fallback (a machine built before the
// drive policy existed, or one whose D: was unavailable) has to follow the
// agent to the newly resolved root -- the edit copy is the user's only copy.
func TestAdoptCarriesMirrorToNewRoot(t *testing.T) {
	isolateUserConfig(t)
	source, err := config.FallbackRoot()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"view/" + adoptRepo + "/report.docx":                        "view-copy",
		"edit/" + adoptRepo + "/report.docx":                        "working-copy",
		".cloudfile-records/local-edit/" + adoptRepo + "/aaaa.json": `{"path":"/report.docx"}`,
		".cloudfile-records/local-view/" + adoptRepo + "/bbbb.json": `{"path":"/report.docx"}`,
	}
	writeMirror(t, source, files)
	target := filepath.Join(t.TempDir(), "workspace")

	adopted, err := Adopt(target)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !adopted {
		t.Fatal("a populated source must be carried over")
	}
	for name, content := range files {
		if got := readFile(t, filepath.Join(target, filepath.FromSlash(name))); got != content {
			t.Fatalf("%s moved with content %q, want %q", name, got, content)
		}
	}
	for _, sub := range mirrorSubtrees {
		if _, err := os.Stat(filepath.Join(source, sub)); !os.IsNotExist(err) {
			t.Fatalf("source %s must be gone after a verified move (err=%v)", sub, err)
		}
	}
	if got := config.RecordedMirrorRoot(); got != target {
		t.Fatalf("marker is %q, want %q", got, target)
	}
	if _, err := os.Stat(filepath.Join(target, ".cloudfile-records", adoptReportName)); err != nil {
		t.Fatalf("adoption report missing: %v", err)
	}
}

// Two mirrors that both hold content are never merged: guessing which copy is
// newer would be exactly the silent data loss this whole path exists to avoid.
func TestAdoptNeverMergesTwoPopulatedMirrors(t *testing.T) {
	isolateUserConfig(t)
	source, err := config.FallbackRoot()
	if err != nil {
		t.Fatal(err)
	}
	writeMirror(t, source, map[string]string{"edit/" + adoptRepo + "/from-source": "source"})
	target := filepath.Join(t.TempDir(), "workspace")
	writeMirror(t, target, map[string]string{"edit/" + adoptRepo + "/from-target": "target"})

	adopted, err := Adopt(target)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted {
		t.Fatal("a populated target must win instead of being merged into")
	}
	if got := readFile(t, filepath.Join(source, "edit", adoptRepo, "from-source")); got != "source" {
		t.Fatalf("source copy was disturbed: %q", got)
	}
	if got := readFile(t, filepath.Join(target, "edit", adoptRepo, "from-target")); got != "target" {
		t.Fatalf("target copy was disturbed: %q", got)
	}
	// The marker still moves on, so a flapping drive cannot retry forever.
	if got := config.RecordedMirrorRoot(); got != target {
		t.Fatalf("marker is %q, want %q", got, target)
	}
}

// Once the marker names the current root, resolving must stay a quiet no-op --
// this runs on every session and every status query.
func TestAdoptIsANoOpWhenMarkerMatches(t *testing.T) {
	isolateUserConfig(t)
	root := filepath.Join(t.TempDir(), "workspace")
	writeMirror(t, root, map[string]string{"edit/" + adoptRepo + "/kept": "value"})
	if err := config.RecordMirrorRoot(root); err != nil {
		t.Fatal(err)
	}
	adopted, err := Adopt(root)
	if err != nil || adopted {
		t.Fatalf("expected a quiet no-op, got adopted=%v err=%v", adopted, err)
	}
	if got := readFile(t, filepath.Join(root, "edit", adoptRepo, "kept")); got != "value" {
		t.Fatalf("mirror was touched: %q", got)
	}
}

// An empty source (first run, or an unplugged drive) must not be an error, and
// must still record the new home.
func TestAdoptRecordsRootWhenThereIsNothingToCarry(t *testing.T) {
	isolateUserConfig(t)
	root := filepath.Join(t.TempDir(), "workspace")
	adopted, err := Adopt(root)
	if err != nil || adopted {
		t.Fatalf("expected a quiet record, got adopted=%v err=%v", adopted, err)
	}
	if got := config.RecordedMirrorRoot(); got != root {
		t.Fatalf("marker is %q, want %q", got, root)
	}
}

// copyMirror is the last line of defence against clobbering a file the agent
// downloaded while the move was in flight.
func TestCopyMirrorNeverOverwritesExistingFiles(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	writeMirror(t, source, map[string]string{
		"edit/" + adoptRepo + "/taken.docx": "from-source",
		"edit/" + adoptRepo + "/fresh.docx": "from-source",
	})
	writeMirror(t, target, map[string]string{"edit/" + adoptRepo + "/taken.docx": "from-target"})

	copied, skipped, err := copyMirror(source, target)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if copied != 1 || skipped != 1 {
		t.Fatalf("copied=%d skipped=%d, want 1 and 1", copied, skipped)
	}
	if got := readFile(t, filepath.Join(target, "edit", adoptRepo, "taken.docx")); got != "from-target" {
		t.Fatalf("existing file was overwritten with %q", got)
	}
	if got := readFile(t, filepath.Join(target, "edit", adoptRepo, "fresh.docx")); got != "from-source" {
		t.Fatalf("new file was not copied: %q", got)
	}
}

// Only the three mirror subtrees count, so unrelated files in the same
// directory neither migrate nor block a move.
func TestMirrorSizeCountsOnlyMirrorSubtrees(t *testing.T) {
	root := t.TempDir()
	writeMirror(t, root, map[string]string{
		"notes.txt":                "not part of a mirror",
		"edit/" + adoptRepo + "/a": "one",
		"view/" + adoptRepo + "/b": "two",
	})
	files, bytes := mirrorSize(root)
	if files != 2 || bytes != 6 {
		t.Fatalf("mirrorSize = (%d, %d), want (2, 6)", files, bytes)
	}
}

// A machine that already ran an older build has its mirror in a superseded
// default location (Windows used to fall back to the roaming %AppData%).  Moving
// the default must carry those files over instead of stranding them.
func TestAdoptCarriesMirrorFromASupersededRoot(t *testing.T) {
	isolateUserConfig(t)
	superseded := config.SupersededFallbackRoots()
	if len(superseded) == 0 {
		t.Skip("this platform never used another default root")
	}
	writeMirror(t, superseded[0], map[string]string{
		"edit/" + adoptRepo + "/old-work.docx": "older-build-work",
	})
	target := filepath.Join(t.TempDir(), "workspace")

	adopted, err := Adopt(target)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !adopted {
		t.Fatal("a superseded mirror must be carried over")
	}
	if got := readFile(t, filepath.Join(target, "edit", adoptRepo, "old-work.docx")); got != "older-build-work" {
		t.Fatalf("carried content = %q", got)
	}
}

func TestSamePathFoldsCaseOnlyOnWindows(t *testing.T) {
	if !samePath(`D:\CloudFileAgent2\workspace`, `D:\CloudFileAgent2\workspace\`) {
		t.Fatal("trailing separator must not matter")
	}
	if samePath("", "/tmp/x") {
		t.Fatal("an empty path is never the same as a real one")
	}
}
