package runner

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const testRepoID = "11111111-2222-3333-4444-555555555555"

func mirrorServer(t *testing.T, body string) (*httptest.Server, *int) {
	t.Helper()
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/download" {
			http.NotFound(w, r)
			return
		}
		downloads++
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &downloads
}

// pathAwareServer answers with a body derived from the requested path, so two
// different library paths can be told apart after they land side by side.
func pathAwareServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		fmt.Fprintf(w, "body of %s\n", r.URL.Path)
	}))
	t.Cleanup(server.Close)
	return server, &downloads
}

func claimedFor(server *httptest.Server, mode, path, fileID string) session.Claimed {
	return session.Claimed{
		SessionID: testRepoID,
		Mode:      mode,
		RepoID:    testRepoID,
		Path:      path,
		FileID:    fileID,
		File: session.File{
			Name:       filepath.Base(path),
			ContentURL: server.URL + "/download",
		},
	}
}

// pathAwareClaim serves the claim's own library path, so its body is unique.
func pathAwareClaim(server *httptest.Server, mode, path, fileID string) session.Claimed {
	claimed := claimedFor(server, mode, path, fileID)
	claimed.File.ContentURL = server.URL + path
	return claimed
}

func libraryDir(t *testing.T, root string, claimed session.Claimed) string {
	t.Helper()
	dir, err := workspace.Dir(root, claimed.Mode, claimed.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func materializeFor(t *testing.T, root string, claimed session.Claimed, action string) string {
	t.Helper()
	path, err := materialize(root, claimed, claimed.RepoID, libraryDir(t, root, claimed), action)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestManualEditKeepsLocalChangesUntilExplicitOverwrite(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeEdit, "/plan.docx", "obj-a")
	localPath := materializeFor(t, root, claimed, "")

	if err := os.WriteFile(localPath, []byte("local changes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if again := materializeFor(t, root, claimed, ""); again != localPath {
		t.Fatalf("the working copy moved to %s", again)
	}
	if got := readFile(t, localPath); got != "local changes\n" || *downloads != 1 {
		t.Fatalf("manual edit was overwritten: data=%q downloads=%d", got, *downloads)
	}
	if _, err := materialize(root, claimed, claimed.RepoID, filepath.Dir(localPath), "overwrite"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, localPath); got != "server version\n" || *downloads != 2 {
		t.Fatalf("explicit overwrite failed: data=%q downloads=%d", got, *downloads)
	}
}

// The whole point of the split: a view open must not be able to clobber the
// working copy the user is still editing.
func TestViewOpenNeverOverwritesAHalfFinishedEdit(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	editClaim := claimedFor(server, workspace.ModeEdit, "/dir/plan.dwg", "obj-a")
	viewClaim := claimedFor(server, workspace.ModeView, "/dir/plan.dwg", "obj-a")

	editPath := materializeFor(t, root, editClaim, "")
	if err := os.WriteFile(editPath, []byte("half finished edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	viewPath := materializeFor(t, root, viewClaim, "")

	if viewPath == editPath {
		t.Fatalf("view and edit share one path: %s", editPath)
	}
	// Same library path, so the same file name - in two separate folders.
	if filepath.Base(viewPath) != filepath.Base(editPath) {
		t.Fatalf("names differ: %s vs %s", viewPath, editPath)
	}
	if got := readFile(t, editPath); got != "half finished edit\n" {
		t.Fatalf("viewing overwrote the local edit: %q", got)
	}
	if *downloads != 2 {
		t.Fatalf("expected one download per subtree, got %d", *downloads)
	}
	viewInfo, err := os.Stat(viewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.IsReadOnly(viewInfo) {
		t.Fatalf("view replica must stay read-only, got %v", viewInfo.Mode())
	}
	editInfo, err := os.Stat(editPath)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.IsReadOnly(editInfo) {
		t.Fatalf("edit copy must stay writable, got %v", editInfo.Mode())
	}
}

func TestViewReplicaIsReusedOnlyWhileItStillMatches(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeView, "/plan.dwg", "obj-a")
	localPath := materializeFor(t, root, claimed, "")

	if again := materializeFor(t, root, claimed, ""); again != localPath {
		t.Fatalf("the replica moved to %s", again)
	}
	if *downloads != 1 {
		t.Fatalf("an unchanged replica should be reused, downloads=%d", *downloads)
	}

	// The server moved on: the replica is stale even though it is untouched.
	claimed.FileID = "obj-b"
	if again := materializeFor(t, root, claimed, ""); again != localPath {
		t.Fatalf("a stale replica must be re-fetched in place, got %s", again)
	}
	if *downloads != 2 {
		t.Fatalf("a stale replica should be re-fetched, downloads=%d", *downloads)
	}

	// The replica itself was altered behind our back.
	if err := os.Chmod(localPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := materialize(root, claimed, claimed.RepoID, filepath.Dir(localPath), ""); err != nil {
		t.Fatal(err)
	}
	if *downloads != 3 {
		t.Fatalf("a tampered replica should be re-fetched, downloads=%d", *downloads)
	}
	if got := readFile(t, localPath); got != "server version\n" {
		t.Fatalf("re-fetched replica has wrong content: %q", got)
	}
}

// Two library paths that happen to end in the same file name are different
// files: the flat folder cannot hold both under one name, so the second one is
// stored the way Windows would ("plan (1).dwg") and the library path stays the
// identity.
func TestCollidingFileNamesGetTheWindowsSuffix(t *testing.T) {
	server, _ := pathAwareServer(t)
	root := t.TempDir()
	first := pathAwareClaim(server, workspace.ModeView, "/design/plan.dwg", "obj-a")
	second := pathAwareClaim(server, workspace.ModeView, "/other/plan.dwg", "obj-b")

	firstPath := materializeFor(t, root, first, "")
	secondPath := materializeFor(t, root, second, "")

	if filepath.Base(firstPath) != "plan.dwg" {
		t.Fatalf("first name = %s, want plan.dwg", filepath.Base(firstPath))
	}
	if filepath.Base(secondPath) != "plan (1).dwg" {
		t.Fatalf("second name = %s, want plan (1).dwg", filepath.Base(secondPath))
	}
	if got, want := readFile(t, secondPath), "body of /other/plan.dwg\n"; got != want {
		t.Fatalf("second copy holds %q, want %q", got, want)
	}
	// Re-opening the first path still finds its own copy, not the neighbour.
	if again := materializeFor(t, root, first, ""); again != firstPath {
		t.Fatalf("first path resolved to %s, want %s", again, firstPath)
	}
}

// A recorded name is the file's address: deleting the file must bring it back
// under the same name, not under a new "(2)".
func TestARecordedNameIsReusedAfterTheFileIsDeleted(t *testing.T) {
	server, _ := pathAwareServer(t)
	root := t.TempDir()
	first := pathAwareClaim(server, workspace.ModeEdit, "/design/plan.dwg", "obj-a")
	second := pathAwareClaim(server, workspace.ModeEdit, "/other/plan.dwg", "obj-b")

	firstPath := materializeFor(t, root, first, "")
	materializeFor(t, root, second, "")

	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	again := materializeFor(t, root, first, "")
	if again != firstPath {
		t.Fatalf("recreated at %s, want the recorded %s", again, firstPath)
	}
	if got, want := readFile(t, again), "body of /design/plan.dwg\n"; got != want {
		t.Fatalf("recreated copy holds %q, want %q", got, want)
	}
}

// A read-only replica must be replaceable: Windows refuses to open a read-only
// file for writing, so the download has to clear the bit first.
func TestFetchReplacesAReadOnlyReplica(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeView, "/plan.dwg", "obj-a")
	localPath := materializeFor(t, root, claimed, "")

	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.IsReadOnly(info) {
		t.Fatalf("a view replica should be read-only, got %v", info.Mode())
	}

	// The server moved on; the replica is read-only and must still be replaced
	// in place rather than beside it.
	claimed.FileID = "obj-b"
	if got := materializeFor(t, root, claimed, ""); got != localPath {
		t.Fatalf("path = %s, want %s", got, localPath)
	}
	if *downloads != 2 {
		t.Fatalf("a read-only replica was not replaced, downloads=%d", *downloads)
	}
	if got := readFile(t, localPath); got != "server version\n" {
		t.Fatalf("replica holds %q", got)
	}
}

// The flat folder cannot prove which library path a file belongs to, and the
// fingerprint is the only evidence. A file this agent never downloaded is
// therefore left alone rather than adopted or overwritten: it may belong to a
// different library path, or to the user.
func TestAForeignFileIsNeverAdoptedAsTheLocalCopy(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeView, "/plan.dwg", "obj-a")
	dir := libraryDir(t, root, claimed)
	foreign := filepath.Join(dir, "plan.dwg")
	if err := os.WriteFile(foreign, []byte("someone else's file\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got := materializeFor(t, root, claimed, "")
	if filepath.Base(got) != "plan (1).dwg" {
		t.Fatalf("name = %s, want plan (1).dwg", filepath.Base(got))
	}
	if body := readFile(t, foreign); body != "someone else's file\n" {
		t.Fatalf("the foreign file was touched: %q", body)
	}
	if *downloads != 1 {
		t.Fatalf("downloads = %d, want 1", *downloads)
	}
}
