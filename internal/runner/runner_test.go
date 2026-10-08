package runner

import (
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

func localPathFor(t *testing.T, root string, claimed session.Claimed) string {
	t.Helper()
	localPath, err := workspace.Path(root, claimed.Mode, claimed.RepoID, claimed.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
		t.Fatal(err)
	}
	return localPath
}

func TestManualEditKeepsLocalChangesUntilExplicitOverwrite(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeEdit, "/plan.docx", "obj-a")
	localPath := localPathFor(t, root, claimed)

	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("local changes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(localPath)
	if err != nil || string(data) != "local changes\n" || *downloads != 1 {
		t.Fatalf("manual edit was overwritten: data=%q downloads=%d err=%v", data, *downloads, err)
	}
	if err := materialize(root, claimed, claimed.RepoID, localPath, "overwrite"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(localPath)
	if err != nil || string(data) != "server version\n" || *downloads != 2 {
		t.Fatalf("explicit overwrite failed: data=%q downloads=%d err=%v", data, *downloads, err)
	}
}

// The whole point of the split: a view open must not be able to clobber the
// working copy the user is still editing.
func TestViewOpenNeverOverwritesAHalfFinishedEdit(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	editClaim := claimedFor(server, workspace.ModeEdit, "/dir/plan.dwg", "obj-a")
	viewClaim := claimedFor(server, workspace.ModeView, "/dir/plan.dwg", "obj-a")

	editPath := localPathFor(t, root, editClaim)
	if err := materialize(root, editClaim, editClaim.RepoID, editPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(editPath, []byte("half finished edit\n"), 0600); err != nil {
		t.Fatal(err)
	}

	viewPath := localPathFor(t, root, viewClaim)
	if err := materialize(root, viewClaim, viewClaim.RepoID, viewPath, ""); err != nil {
		t.Fatal(err)
	}

	if viewPath == editPath {
		t.Fatalf("view and edit share one path: %s", editPath)
	}
	data, err := os.ReadFile(editPath)
	if err != nil || string(data) != "half finished edit\n" {
		t.Fatalf("viewing overwrote the local edit: data=%q err=%v", data, err)
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
	localPath := localPathFor(t, root, claimed)

	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	if *downloads != 1 {
		t.Fatalf("an unchanged replica should be reused, downloads=%d", *downloads)
	}

	// The server moved on: the replica is stale even though it is untouched.
	claimed.FileID = "obj-b"
	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
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
	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	if *downloads != 3 {
		t.Fatalf("a tampered replica should be re-fetched, downloads=%d", *downloads)
	}
	data, err := os.ReadFile(localPath)
	if err != nil || string(data) != "server version\n" {
		t.Fatalf("re-fetched replica has wrong content: %q err=%v", data, err)
	}
}

// A read-only replica must be replaceable: Windows refuses to open a read-only
// file for writing, so the download has to clear the bit first.
func TestFetchReplacesAReadOnlyReplica(t *testing.T) {
	server, downloads := mirrorServer(t, "server version\n")
	root := t.TempDir()
	claimed := claimedFor(server, workspace.ModeView, "/plan.dwg", "obj-a")
	localPath := localPathFor(t, root, claimed)

	if err := os.WriteFile(localPath, []byte("previous\n"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localPath, 0400); err != nil {
		t.Fatal(err)
	}
	if err := materialize(root, claimed, claimed.RepoID, localPath, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(localPath)
	if err != nil || string(data) != "server version\n" || *downloads != 1 {
		t.Fatalf("read-only replica was not replaced: data=%q downloads=%d err=%v", data, *downloads, err)
	}
}
