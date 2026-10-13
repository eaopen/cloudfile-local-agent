package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

const testRepo = "c66005f6-36bc-45e1-bc8f-88bd1ec13ded"

// The mirror is flat: the library's folder tree is not reproduced, so a deeply
// nested server path still lands directly inside the library folder.
func TestPathIsFlatInsideTheLibraryFolder(t *testing.T) {
	root := t.TempDir()
	view, err := Path(root, ModeView, testRepo, "/design/plan/plan.dwg")
	if err != nil {
		t.Fatal(err)
	}
	edit, err := Path(root, ModeEdit, testRepo, "/design/plan/plan.dwg")
	if err != nil {
		t.Fatal(err)
	}
	if view == edit {
		t.Fatalf("view and edit resolved to the same path: %s", view)
	}
	want := filepath.Join(root, "view", testRepo, "plan.dwg")
	if view != want {
		t.Fatalf("view path = %s, want %s", view, want)
	}
	wantEdit := filepath.Join(root, "edit", testRepo, "plan.dwg")
	if edit != wantEdit {
		t.Fatalf("edit path = %s, want %s", edit, wantEdit)
	}
}

// The library folder is the first level below the mode, so both the per-file
// path and the "open the folder" target agree on it.
func TestDirIsTheFirstLevelBelowTheMode(t *testing.T) {
	root := t.TempDir()
	for _, mode := range []string{ModeView, ModeEdit} {
		dir, err := Dir(root, mode, testRepo)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, Subdir(mode), testRepo)
		if dir != want {
			t.Fatalf("Dir(%s) = %s, want %s", mode, dir, want)
		}
	}
	if _, err := Dir(root, ModeEdit, ""); err == nil {
		t.Fatal("an empty library identifier must be rejected")
	}
}

func TestPathOfEmptyServerPathIsTheLibraryFolder(t *testing.T) {
	root := t.TempDir()
	target, err := Path(root, ModeEdit, testRepo, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "edit", testRepo); target != want {
		t.Fatalf("library folder = %s, want %s", target, want)
	}
}

// A hostile path is flattened to its last segment, which can never leave the
// library folder: there are no subdirectories to escape through.
func TestPathStaysInsideTheLibraryFolder(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "edit", testRepo)
	for _, hostile := range []string{"/../../outside.dwg", "/design/../../outside.dwg", "../outside.dwg"} {
		target, err := Path(root, ModeEdit, testRepo, hostile)
		if err != nil {
			t.Fatalf("%q was rejected as unsafe: %v", hostile, err)
		}
		if filepath.Dir(target) != base {
			t.Fatalf("%q escaped to %s", hostile, target)
		}
	}
}

func TestPathRejectsAnUnsafeLibraryIdentifier(t *testing.T) {
	root := t.TempDir()
	for _, identifier := range []string{"", "..", ".", "../escape", ".cloudfile-meta", `a\b`, "a/b", "c:evil"} {
		if _, err := Path(root, ModeEdit, identifier, "/plan.dwg"); err == nil {
			t.Fatalf("library identifier %q was accepted", identifier)
		}
	}
}

func TestSubdirFallsBackToTheEditableSubtree(t *testing.T) {
	if got := Subdir(ModeView); got != ViewDir {
		t.Fatalf("view mode mapped to %q", got)
	}
	if got := Subdir(ModeEdit); got != EditDir {
		t.Fatalf("edit mode mapped to %q", got)
	}
	if got := Subdir(""); got != EditDir {
		t.Fatalf("unknown mode must be editable, got %q", got)
	}
}

func TestServerNameIsTheLastSegment(t *testing.T) {
	for _, c := range []struct {
		path, want string
	}{
		{"/design/plan/plan.dwg", "plan.dwg"},
		{"/plan.dwg", "plan.dwg"},
		{"/设计/图纸.dwg", "图纸.dwg"},
	} {
		got, err := ServerName(c.path)
		if err != nil {
			t.Fatalf("ServerName(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Fatalf("ServerName(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	if _, err := ServerName("/design/"); err == nil {
		t.Fatal("a folder path has no file name")
	}
}

// Windows naming: an occupied name gets " (1)", then " (2)", and an existing
// suffix is counted up rather than appended to.
func TestUniqueNameCountsUpLikeWindows(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "report.pdf"), "a\n", 0600)

	got, err := UniqueName(dir, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got != "report (1).pdf" {
		t.Fatalf("first clash = %q, want report (1).pdf", got)
	}

	writeFile(t, filepath.Join(dir, "report (1).pdf"), "b\n", 0600)
	got, err = UniqueName(dir, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got != "report (2).pdf" {
		t.Fatalf("second clash = %q, want report (2).pdf", got)
	}

	// The trap this rule exists for: the name offered for a collision may
	// already carry a suffix, and it must be counted up, not nested.
	got, err = UniqueName(dir, "report (1).pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got != "report (2).pdf" {
		t.Fatalf("suffixed clash = %q, want report (2).pdf (never report (1) (1).pdf)", got)
	}

	// With the plain name free again the suffixed name is still offered as-is
	// when nothing claims it.
	free := t.TempDir()
	got, err = UniqueName(free, "report (1).pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got != "report (1).pdf" {
		t.Fatalf("an unclaimed name = %q, want it untouched", got)
	}
}

func TestUniqueNameKeepsTheExtension(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "archive.tar.gz"), "a\n", 0600)
	got, err := UniqueName(dir, "archive.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "archive.tar (1).gz" {
		t.Fatalf("got %q, want archive.tar (1).gz (extension = last dot, like Windows)", got)
	}

	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "noext"), "a\n", 0600)
	got, err = UniqueName(dir2, "noext")
	if err != nil {
		t.Fatal(err)
	}
	if got != "noext (1)" {
		t.Fatalf("got %q, want noext (1)", got)
	}
}

func TestUniqueNameRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "c:evil"} {
		if _, err := UniqueName(dir, name); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
}

// The name on disk comes from the record, because the flat folder cannot encode
// the library path: that is the whole reason a collision can be resolved.
func TestFileNamePrefersTheRecordedName(t *testing.T) {
	root := t.TempDir()

	name, recorded := FileName(root, ModeEdit, testRepo, "/design/plan.dwg")
	if recorded {
		t.Fatal("a file with no record must not be reported as recorded")
	}
	if name != "plan.dwg" {
		t.Fatalf("unrecorded name = %q, want plan.dwg", name)
	}

	if err := WriteRecord(root, ModeEdit, testRepo, "/design/plan.dwg", Record{
		RepoID: testRepo, Path: "/design/plan.dwg", Mode: ModeEdit, Name: "plan (1).dwg",
	}); err != nil {
		t.Fatal(err)
	}
	name, recorded = FileName(root, ModeEdit, testRepo, "/design/plan.dwg")
	if !recorded || name != "plan (1).dwg" {
		t.Fatalf("recorded name = %q recorded=%v, want plan (1).dwg", name, recorded)
	}
	// The same file name in another library path is a different file.
	other, otherRecorded := FileName(root, ModeEdit, testRepo, "/other/plan.dwg")
	if otherRecorded || other != "plan.dwg" {
		t.Fatalf("other path = %q recorded=%v, want a fresh plan.dwg", other, otherRecorded)
	}
}

func TestWriteRecordRejectsAnUnsafeName(t *testing.T) {
	root := t.TempDir()
	if err := WriteRecord(root, ModeEdit, testRepo, "/plan.dwg", Record{Name: "../escape.dwg"}); err == nil {
		t.Fatal("a record pointing outside the library folder must be refused")
	}
}

func TestPathUsesTheRecordedName(t *testing.T) {
	root := t.TempDir()
	if err := WriteRecord(root, ModeView, testRepo, "/design/plan.dwg", Record{Name: "plan (1).dwg"}); err != nil {
		t.Fatal(err)
	}
	target, err := Path(root, ModeView, testRepo, "/design/plan.dwg")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "view", testRepo, "plan (1).dwg"); target != want {
		t.Fatalf("path = %s, want %s", target, want)
	}
	if _, err := os.Stat(filepath.Join(root, "view", testRepo)); err == nil {
		t.Fatal("resolving a path must not create the library folder")
	}
}
