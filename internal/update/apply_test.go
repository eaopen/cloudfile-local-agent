package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestApplyEndToEnd exercises the full self-update flow against a mock static
// update source: fetch manifest -> download+verify binary -> rewrite manifest
// path -> prune old versions.
func TestApplyEndToEnd(t *testing.T) {
	const newBinary = "fake-binary-0.4.0\n"
	sum := sha256Of(newBinary)

	mux := http.NewServeMux()
	mux.HandleFunc("/cloudfile-updates/update.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{
			Version:     "0.4.0",
			SHA256:      sum,
			DownloadURL: "./cloudfile-local-agent-0.4.0.exe",
			MinVersion:  "0.3.0",
		})
	})
	mux.HandleFunc("/cloudfile-updates/cloudfile-local-agent-0.4.0.exe", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(newBinary))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	source := server.URL + "/cloudfile-updates/update.json"
	manifest, err := Fetch(source)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "com.cloudfile.local_agent.json")
	_ = os.WriteFile(manifestPath, []byte(`{
    "name": "com.cloudfile.local_agent",
    "description": "CloudFile Local Agent",
    "path": "OLD",
    "type": "stdio",
    "allowed_origins": ["chrome-extension://test/"]
}`), 0600)

	installed, err := applyWithPaths(source, manifest, dir, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// Installed binary has the versioned name.
	if filepath.Base(installed) != "cloudfile-local-agent-0.4.0.exe" {
		t.Fatalf("unexpected installed path %q", installed)
	}
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != newBinary {
		t.Fatalf("installed binary mismatch: %q err=%v", data, err)
	}
	// Manifest path was rewritten while other fields preserved.
	var updated map[string]any
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["path"] != installed {
		t.Fatalf("manifest path = %v, want %v", updated["path"], installed)
	}
	if updated["name"] != "com.cloudfile.local_agent" || updated["type"] != "stdio" {
		t.Fatalf("manifest fields lost: %v", updated)
	}
}

func sha256Of(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
