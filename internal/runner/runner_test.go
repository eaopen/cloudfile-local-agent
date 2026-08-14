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

func TestDownloadHeartbeatAndWritebackProtocol(t *testing.T) {
	const capability = "fenced-capability"
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			_, _ = io.WriteString(w, "version one\n")
		case "/heartbeat":
			if r.Method != http.MethodPatch ||
				r.Header.Get("Authorization") != "Bearer "+capability {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/writeback":
			if r.Method != http.MethodPut ||
				r.Header.Get("Authorization") != "Bearer "+capability {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			uploaded, err = io.ReadAll(file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "plan.docx")
	if err := download(server.URL+"/download", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "version one\n" {
		t.Fatalf("downloaded %q, err=%v", data, err)
	}
	if err := os.WriteFile(path, []byte("version two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeback := session.Writeback{
		ContentURL: server.URL + "/writeback", HeartbeatURL: server.URL + "/heartbeat",
		Capability: capability,
	}
	if err := renew(writeback); err != nil {
		t.Fatal(err)
	}
	if err := upload(path, writeback); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "version two\n" {
		t.Fatalf("uploaded %q", uploaded)
	}
}

func TestWritebackRequiresSuccessfulNoContentResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stale generation", http.StatusConflict)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "plan.docx")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	err := upload(path, session.Writeback{ContentURL: server.URL})
	if err == nil {
		t.Fatal("expected rejected writeback")
	}
}
