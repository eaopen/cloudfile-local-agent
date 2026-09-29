package editing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRetainsWorkingCopyAndEnforcesLimit(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "drawing.dwg")
	if err := os.WriteFile(source, []byte("saved content"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := CreateSnapshot(source, dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 || snapshot == source {
		t.Fatal("invalid snapshot identity")
	}
	if err := os.WriteFile(source, []byte("next edit"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil || string(data) != "saved content" {
		t.Fatal("snapshot changed with working copy")
	}
	if _, _, err = CreateSnapshot(source, dir, 2); err == nil {
		t.Fatal("size limit ignored")
	}
	data, err = os.ReadFile(source)
	if err != nil || string(data) != "next edit" {
		t.Fatal("working copy lost")
	}
}
