package runner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/session"
)

func TestManualEditKeepsLocalChangesUntilExplicitOverwrite(t *testing.T) {
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/download" {
			http.NotFound(w, r)
			return
		}
		downloads++
		_, _ = io.WriteString(w, "server version\n")
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "plan.docx")
	claimed := session.Claimed{Mode: "local-edit", File: session.File{
		Name: "plan.docx", ContentURL: server.URL + "/download",
	}}
	if err := reuseOrDownload(claimed, path, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local changes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reuseOrDownload(claimed, path, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "local changes\n" || downloads != 1 {
		t.Fatalf("manual edit was overwritten: data=%q downloads=%d err=%v", data, downloads, err)
	}
	if err := reuseOrDownload(claimed, path, "overwrite"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "server version\n" || downloads != 2 {
		t.Fatalf("explicit overwrite failed: data=%q downloads=%d err=%v", data, downloads, err)
	}
}
