package workspace

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	root := t.TempDir()
	written := Record{RepoID: testRepo, Path: "/design/plan.dwg", Mode: ModeEdit, Name: "plan (1).dwg", FileID: "obj-a", Digest: "abc", Size: 12}
	if err := WriteRecord(root, ModeEdit, testRepo, "/design/plan.dwg", written); err != nil {
		t.Fatal(err)
	}
	loaded, found := ReadRecord(root, ModeEdit, testRepo, "/design/plan.dwg")
	if !found {
		t.Fatal("record was not readable")
	}
	if loaded.Name != "plan (1).dwg" {
		t.Fatalf("the local file name was lost: %+v", loaded)
	}
	if loaded.FileID != "obj-a" || loaded.Digest != "abc" || loaded.Size != 12 || loaded.StoredAt == 0 {
		t.Fatalf("record round trip lost data: %+v", loaded)
	}
}

func TestRecordIsScopedToModeAndFolder(t *testing.T) {
	root := t.TempDir()
	if err := WriteRecord(root, ModeEdit, testRepo, "/a/plan.dwg", Record{Digest: "edit-a"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(root, ModeEdit, testRepo, "/b/plan.dwg", Record{Digest: "edit-b"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(root, ModeView, testRepo, "/a/plan.dwg", Record{Digest: "view-a"}); err != nil {
		t.Fatal(err)
	}

	for _, expectation := range []struct {
		mode string
		path string
		want string
	}{
		{ModeEdit, "/a/plan.dwg", "edit-a"},
		{ModeEdit, "/b/plan.dwg", "edit-b"},
		{ModeView, "/a/plan.dwg", "view-a"},
	} {
		loaded, found := ReadRecord(root, expectation.mode, testRepo, expectation.path)
		if !found || loaded.Digest != expectation.want {
			t.Fatalf("%s %s = %+v found=%v, want %s", expectation.mode, expectation.path, loaded, found, expectation.want)
		}
	}
	if _, found := ReadRecord(root, ModeView, testRepo, "/b/plan.dwg"); found {
		t.Fatal("a missing record was reported as present")
	}
}

func TestCorruptRecordIsTreatedAsMissing(t *testing.T) {
	root := t.TempDir()
	path, err := metaPath(root, ModeEdit, testRepo, "/plan.dwg")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "{not json", 0600)

	if _, found := ReadRecord(root, ModeEdit, testRepo, "/plan.dwg"); found {
		t.Fatal("a corrupt record must be treated as no record")
	}
}

func TestDigestFileMatchesTheStandardHashes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plan.dwg")
	body := []byte("server version\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}

	sha1Sum := sha1.Sum(body)
	sha256Sum := sha256.Sum256(body)
	gotSha1, gotSha256, err := Hashes(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotSha1 != hex.EncodeToString(sha1Sum[:]) {
		t.Fatalf("sha1 = %s", gotSha1)
	}
	if gotSha256 != hex.EncodeToString(sha256Sum[:]) {
		t.Fatalf("sha256 = %s", gotSha256)
	}
	digest, err := DigestFile(path)
	if err != nil || digest != gotSha256 {
		t.Fatalf("DigestFile = %s err=%v", digest, err)
	}
}

func TestIsReadOnlyTracksTheWriteBit(t *testing.T) {
	root := t.TempDir()
	readOnly := filepath.Join(root, "view.dwg")
	writeFile(t, readOnly, "view copy\n", 0400)
	writable := filepath.Join(root, "edit.dwg")
	writeFile(t, writable, "edited locally\n", 0600)

	readOnlyInfo, err := os.Stat(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !IsReadOnly(readOnlyInfo) {
		t.Fatalf("0400 file reported writable: %v", readOnlyInfo.Mode())
	}
	writableInfo, err := os.Stat(writable)
	if err != nil {
		t.Fatal(err)
	}
	if IsReadOnly(writableInfo) {
		t.Fatalf("0600 file reported read-only: %v", writableInfo.Mode())
	}
}
