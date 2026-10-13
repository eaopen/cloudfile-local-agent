//go:build darwin

package update

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMacUpdatePreservesManifestSymlinkAndExecutableMode(t *testing.T) {
	const payload = "candidate"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(payload)) }))
	defer server.Close()
	root := t.TempDir()
	browser := filepath.Join(root, "browser.json")
	link := filepath.Join(root, "host.json")
	os.WriteFile(browser, []byte(`{"name":"com.cloudfile.agent2","type":"stdio","path":"old"}`), 0600)
	if err := os.Symlink(browser, link); err != nil {
		t.Fatal(err)
	}
	manifest := &Manifest{Version: "2.0.2", DownloadURL: server.URL, SHA256: sha256Of(payload)}
	installed, err := applyWithPaths(server.URL, manifest, filepath.Join(root, "bin"), link)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("manifest symlink was replaced")
	}
	binary, err := os.Stat(installed)
	if err != nil || binary.Mode().Perm() != 0700 {
		t.Fatal("update is not private and executable")
	}
	before, _ := os.ReadFile(browser)
	manifest.SHA256 = sha256Of("different")
	if _, err := applyWithPaths(server.URL, manifest, filepath.Join(root, "bin"), link); err == nil {
		t.Fatal("bad digest accepted")
	}
	after, _ := os.ReadFile(browser)
	if string(before) != string(after) {
		t.Fatal("failed update changed browser entry")
	}
}
func TestMacHealthFailureKeepsRegistration(t *testing.T) {
	const payload = "#!/bin/sh\nexit 1\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(payload)) }))
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "host.json")
	original := []byte(`{"name":"com.cloudfile.agent2","path":"old"}`)
	os.WriteFile(path, original, 0600)
	_, err := applyChecked(server.URL, &Manifest{Version: "2.0.2", DownloadURL: server.URL, SHA256: sha256Of(payload)}, filepath.Join(root, "bin"), path, true)
	if err == nil {
		t.Fatal("unhealthy candidate activated")
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != string(original) {
		t.Fatal("failed health check changed registration")
	}
}
