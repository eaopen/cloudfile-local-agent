package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// A legacy mirror has no record of which session wrote each file. The read-only
// marker is the only evidence available, and it must decide the subtree. The
// nested legacy shape is flattened on the way in, and the fingerprint picks up
// the name the file ended up with.
func TestAdoptLegacyMirrorsSplitsOnTheReadOnlyMarker(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, testRepo)
	writeFile(t, filepath.Join(legacy, "notes.txt"), "edited locally\n", 0600)
	writeFile(t, filepath.Join(legacy, "design", "plan.dwg"), "view copy\n", 0400)

	if err := Ensure(root); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("writable legacy file did not land in edit/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "view", testRepo, "plan.dwg")); err != nil {
		t.Fatalf("read-only legacy file did not land flat in view/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "view", testRepo, "design")); !os.IsNotExist(err) {
		t.Fatalf("the legacy folder tree survived the flatten: %v", err)
	}
	if got := recordedName(t, root, ModeView, "/design/plan.dwg"); got != "plan.dwg" {
		t.Fatalf("fingerprint name = %q, want plan.dwg", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy folder survived: %v", err)
	}
	if markerRevision(root) != layoutRevision {
		t.Fatalf("root was not stamped: %q", markerRevision(root))
	}
}

// recordedName reads the name a fingerprint carries for one library path.
func recordedName(t *testing.T, root, mode, serverPath string) string {
	t.Helper()
	record, found := ReadRecord(root, mode, testRepo, serverPath)
	if !found {
		t.Fatalf("no fingerprint for %s %s", mode, serverPath)
	}
	return record.Name
}

// Revision 2 kept the library's folder tree; revision 3 does not. Flattening
// must keep every file (a clash only adds a suffix), re-point each fingerprint
// at its new name, and leave no empty folders behind.
func TestFlattenMirrorsKeepsEveryFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, EditDir, testRepo)
	writeFile(t, filepath.Join(dir, "plan.dwg"), "library root\n", 0600)
	writeFile(t, filepath.Join(dir, "design", "plan.dwg"), "design folder\n", 0600)
	writeFile(t, filepath.Join(dir, "other", "plan.dwg"), "other folder\n", 0600)
	writeFile(t, filepath.Join(dir, "design", "notes.txt"), "notes\n", 0600)
	// A fingerprint written by revision 2: no name yet.
	if err := WriteRecord(root, ModeEdit, testRepo, "/design/notes.txt", Record{
		RepoID: testRepo, Path: "/design/notes.txt", Mode: ModeEdit, FileID: "obj-notes", Digest: "d",
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, markerFile)
	if err := os.WriteFile(marker, []byte("2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(root); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"plan.dwg":     "library root\n",
		"plan (1).dwg": "design folder\n",
		"plan (2).dwg": "other folder\n",
		"notes.txt":    "notes\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s is missing: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s holds %q, want %q", name, got, want)
		}
	}
	if got := recordedName(t, root, ModeEdit, "/design/plan.dwg"); got != "plan (1).dwg" {
		t.Fatalf("/design/plan.dwg -> %q, want plan (1).dwg", got)
	}
	if got := recordedName(t, root, ModeEdit, "/other/plan.dwg"); got != "plan (2).dwg" {
		t.Fatalf("/other/plan.dwg -> %q, want plan (2).dwg", got)
	}
	// An existing fingerprint keeps its content data and gains the name.
	record, found := ReadRecord(root, ModeEdit, testRepo, "/design/notes.txt")
	if !found || record.Name != "notes.txt" || record.FileID != "obj-notes" || record.Digest != "d" {
		t.Fatalf("fingerprint was not preserved: %+v found=%v", record, found)
	}
	// A file with no fingerprint at all still gets a name-only one, so the
	// local copy is not mistaken for a stranger's file.
	if got := recordedName(t, root, ModeEdit, "/plan.dwg"); got != "plan.dwg" {
		t.Fatalf("/plan.dwg -> %q, want plan.dwg", got)
	}
	for _, leftover := range []string{"design", "other"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
			t.Fatalf("empty folder %s survived: %v", leftover, err)
		}
	}
	if markerRevision(root) != layoutRevision {
		t.Fatalf("root was not stamped: %q", markerRevision(root))
	}
}

func TestEnsureLeavesUnrelatedFoldersAlone(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "someones-folder")
	writeFile(t, filepath.Join(other, "keep.txt"), "not a mirror\n", 0600)

	if err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "keep.txt")); err != nil {
		t.Fatalf("unrelated folder was disturbed: %v", err)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, testRepo, "notes.txt"), "edited locally\n", 0600)

	for attempt := 1; attempt <= 3; attempt++ {
		if err := Ensure(root); err != nil {
			t.Fatalf("Ensure run %d failed: %v", attempt, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("file lost after repeated runs: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "edit"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("edit subtree is wrong: entries=%d err=%v", len(entries), err)
	}
}

func TestMigrateMovesTheWholeMirror(t *testing.T) {
	previous := t.TempDir()
	writeFile(t, filepath.Join(previous, "view", testRepo, "plan.dwg"), "view copy\n", 0400)
	writeFile(t, filepath.Join(previous, "edit", testRepo, "notes.txt"), "edited locally\n", 0600)
	writeFile(t, filepath.Join(previous, MetaDir, "edit", testRepo, "notes.txt.json"), "{}\n", 0600)

	current := filepath.Join(t.TempDir(), "CloudFileLocal", "sessions")
	if err := Migrate(previous, current); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(current, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("edit copy did not move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(current, "view", testRepo, "plan.dwg")); err != nil {
		t.Fatalf("view copy did not move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(current, MetaDir, "edit", testRepo, "notes.txt.json")); err != nil {
		t.Fatalf("fingerprints did not move: %v", err)
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatalf("source tree survived the move: %v", err)
	}
}

func TestMigrateRefusesToMergeIntoAnOccupiedTarget(t *testing.T) {
	previous := t.TempDir()
	writeFile(t, filepath.Join(previous, "edit", testRepo, "notes.txt"), "old machine\n", 0600)
	current := t.TempDir()
	writeFile(t, filepath.Join(current, "edit", testRepo, "notes.txt"), "new machine\n", 0600)

	if err := Migrate(previous, current); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(current, "edit", testRepo, "notes.txt"))
	if err != nil || string(kept) != "new machine\n" {
		t.Fatalf("occupied target was overwritten: %q err=%v", kept, err)
	}
	if _, err := os.Stat(filepath.Join(previous, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("source was consumed by a skipped move: %v", err)
	}
}

func TestMigrateIgnoresAnEmptyStampedRoot(t *testing.T) {
	previous := filepath.Join(t.TempDir(), "sessions")
	if err := Ensure(previous); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(previous, "edit", testRepo, "notes.txt"), "edited locally\n", 0600)

	current := filepath.Join(t.TempDir(), "other", "sessions")
	if err := Ensure(current); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(previous, current); err != nil {
		t.Fatal(err)
	}
	// The destination only carried a layout marker, which must not pass for
	// content and block the move.
	if _, err := os.Stat(filepath.Join(current, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("marker-only destination blocked the move: %v", err)
	}
}

func TestMoveTreeReplacesAnExistingEmptyDirectory(t *testing.T) {
	from := filepath.Join(t.TempDir(), "from")
	writeFile(t, filepath.Join(from, "edit", testRepo, "notes.txt"), "edited locally\n", 0600)
	to := filepath.Join(t.TempDir(), "to")
	if err := os.MkdirAll(to, 0700); err != nil {
		t.Fatal(err)
	}

	if err := moveTree(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(to, "edit", testRepo, "notes.txt")); err != nil {
		t.Fatalf("tree did not arrive: %v", err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("source survived: %v", err)
	}
	if _, err := os.Stat(to + ".moving"); !os.IsNotExist(err) {
		t.Fatalf("staging directory was left behind: %v", err)
	}
}
