package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

const repo = "11111111-2222-3333-4444-555555555555"

func TestFlatMirrorUsesPathIdentityAndNeverAdoptsOtherFiles(t *testing.T) {
	root := t.TempDir()
	dir, err := Dir(root, ModeEdit, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "drawing.dwg"), []byte("a different user file"), 0600)
	name, recorded := FileName(root, ModeEdit, repo, "/cad/drawing.dwg")
	if name != "drawing.dwg" || recorded {
		t.Fatal("foreign file considered recorded")
	}
	unique, err := UniqueName(dir, name)
	if err != nil || unique != "drawing (1).dwg" {
		t.Fatalf("wrong duplicate name %s %v", unique, err)
	}
	record := Record{RepoID: repo, Mode: ModeEdit, Path: "/cad/drawing.dwg", Name: unique, FileID: "abc", Digest: "abc", Size: 7}
	if err = WriteRecord(root, ModeEdit, repo, record.Path, record); err != nil {
		t.Fatal(err)
	}
	got, exists := ReadRecord(root, ModeEdit, repo, record.Path)
	if !exists || got.Name != unique || got.StoredAt == 0 {
		t.Fatalf("lost record %+v", got)
	}
	path, err := Path(root, ModeEdit, repo, record.Path)
	if err != nil || path != filepath.Join(dir, unique) {
		t.Fatalf("wrong recorded path %s", path)
	}
	_, exists = ReadRecord(root, ModeView, repo, record.Path)
	if exists {
		t.Fatal("view and edit records crossed")
	}
	for _, bad := range []string{"", "/", "a", "/../secret", "/a/./b", "/a//b", "/a\\b"} {
		if RelPath(bad) != "" {
			t.Fatalf("unsafe remote path %q accepted", bad)
		}
	}
}
