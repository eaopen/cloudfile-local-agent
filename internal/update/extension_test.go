package update

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionUpdateStagesVerifiedZipWithoutAdmin(t *testing.T) {
	var zipBuffer bytes.Buffer
	archive := zip.NewWriter(&zipBuffer)
	member, err := archive.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = member.Write([]byte(`{"manifest_version":3,"name":"CloudFile","version":"1.0.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = archive.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(zipBuffer.Bytes())
	sum := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/updates/extension-update.json":
			json.NewEncoder(w).Encode(ExtensionManifest{Version: "1.0.1", ZIPURL: "./extension.zip", SHA256: sum})
		case "/updates/extension.zip":
			w.Write(zipBuffer.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source, err := ExtensionSourceURL(server.URL + "/updates/update.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := FetchExtension(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "extension")
	if _, err = ApplyExtension(source, manifest, dir); err != nil {
		t.Fatal(err)
	}
	if got := InstalledExtensionVersion(dir); got != "1.0.1" {
		t.Fatalf("wrong version %s", got)
	}
	if _, err = os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	broken := *manifest
	broken.SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err = ApplyExtension(source, &broken, dir); err == nil {
		t.Fatal("tampered archive accepted")
	}
	if got := InstalledExtensionVersion(dir); got != "1.0.1" {
		t.Fatal("failed upgrade damaged installed extension")
	}
}
