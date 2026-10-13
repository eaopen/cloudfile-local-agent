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
	"sync/atomic"
	"testing"
	"time"
)

func sha256OfExtension(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// extensionServer serves a manifest plus the package it points at, and counts
// manifest requests so cache behaviour can be asserted.
func extensionServer(t *testing.T, version string, archive []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/cloudfile-updates/extension-update.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(ExtensionManifest{
			Version: version,
			SHA256:  sha256OfExtension(string(archive)),
			ZipURL:  "./cloudfile-local-session-receiver-" + version + ".zip",
			Notes:   "test release",
		})
	})
	// "receiver.zip" is the deliberately-wrong-checksum target used below.
	for _, name := range []string{
		"cloudfile-local-session-receiver-" + version + ".zip",
		"receiver.zip",
	} {
		mux.HandleFunc("/cloudfile-updates/"+name, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(archive)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &hits
}

func TestExtensionSourceURLDerivesSibling(t *testing.T) {
	cases := []struct {
		source, want string
	}{
		{"http://host:6111/cloudfile-updates/update.json", "http://host:6111/cloudfile-updates/extension-update.json"},
		{"https://portal.example/cloudfile-updates/update.json?v=2", "https://portal.example/cloudfile-updates/extension-update.json"},
		{"http://host:6111/cloudfile-updates", "http://host:6111/cloudfile-updates/extension-update.json"},
		{"http://host:6111/cloudfile-updates/", "http://host:6111/cloudfile-updates/extension-update.json"},
		{"http://host:6111/", "http://host:6111/extension-update.json"},
	}
	for _, c := range cases {
		got, err := ExtensionSourceURL(c.source)
		if err != nil {
			t.Fatalf("ExtensionSourceURL(%q): %v", c.source, err)
		}
		if got != c.want {
			t.Fatalf("ExtensionSourceURL(%q) = %q, want %q", c.source, got, c.want)
		}
	}
}

func TestExtensionSourceURLRejectsRelative(t *testing.T) {
	for _, source := range []string{"", "cloudfile-updates/update.json", "/cloudfile-updates/update.json"} {
		if _, err := ExtensionSourceURL(source); err == nil {
			t.Fatalf("ExtensionSourceURL(%q) should fail", source)
		}
	}
}

func TestExtractZipRejectsEscapingEntry(t *testing.T) {
	for _, name := range []string{"../escape.txt", "nested/../../escape.txt", `..\escape.txt`, "/absolute.txt"} {
		archive := zipBytes(t, map[string]string{name: "boom"})
		path := filepath.Join(t.TempDir(), "pkg.zip")
		if err := os.WriteFile(path, archive, 0600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "dest")
		if err := extractZip(path, dest); err == nil {
			t.Fatalf("extractZip should reject entry %q", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); err == nil {
			t.Fatalf("entry %q escaped the destination", name)
		}
	}
}

func TestExtractZipUnpacksNestedFiles(t *testing.T) {
	archive := zipBytes(t, map[string]string{
		"manifest.json":    `{"version":"1.0.0"}`,
		"icons/icon16.png": "png-bytes",
	})
	path := filepath.Join(t.TempDir(), "pkg.zip")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractZip(path, dest); err != nil {
		t.Fatal(err)
	}
	if got := InstalledExtensionVersion(dest); got != "1.0.0" {
		t.Fatalf("installed version = %q, want 1.0.0", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "icons", "icon16.png")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

func TestApplyExtensionInstallsVerifiedPackage(t *testing.T) {
	archive := zipBytes(t, map[string]string{
		"manifest.json": `{"version":"1.0.0","name":"CloudFile"}`,
		"popup.js":      "console.log('new');",
	})
	server, _ := extensionServer(t, "1.0.0", archive)
	source, err := ExtensionSourceURL(server.URL + "/cloudfile-updates/update.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := FetchExtension(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "extension")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"0.5.7"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExtension(source, manifest, dir); err != nil {
		t.Fatal(err)
	}
	if got := InstalledExtensionVersion(dir); got != "1.0.0" {
		t.Fatalf("installed version = %q, want 1.0.0", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, "popup.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "console.log('new');" {
		t.Fatalf("popup.js = %q, want the new payload", body)
	}
	if _, err := os.Stat(dir + ".staging"); !os.IsNotExist(err) {
		t.Fatal("staging directory was left behind")
	}
}

func TestApplyExtensionKeepsPreviousOnChecksumMismatch(t *testing.T) {
	archive := zipBytes(t, map[string]string{"manifest.json": `{"version":"1.0.0"}`})
	server, _ := extensionServer(t, "1.0.0", archive)
	source, err := ExtensionSourceURL(server.URL + "/cloudfile-updates/update.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "extension")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"0.5.7"}`), 0644); err != nil {
		t.Fatal(err)
	}
	broken := &ExtensionManifest{Version: "1.0.0", SHA256: sha256OfExtension("not the archive"), ZipURL: "./receiver.zip"}
	if _, err := ApplyExtension(source, broken, dir); err == nil {
		t.Fatal("a checksum mismatch must fail the update")
	}
	if got := InstalledExtensionVersion(dir); got != "0.5.7" {
		t.Fatalf("installed version = %q, want the untouched 0.5.7", got)
	}
}

func TestApplyExtensionRequiresSHA256(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extension")
	if _, err := ApplyExtension("http://host.example/update.json", &ExtensionManifest{Version: "1.0.0", ZipURL: "./a.zip"}, dir); err == nil {
		t.Fatal("an unverified extension package must be refused")
	}
}

func TestFetchExtensionCachedReusesFreshCache(t *testing.T) {
	archive := zipBytes(t, map[string]string{"manifest.json": `{"version":"1.0.0"}`})
	server, hits := extensionServer(t, "1.0.0", archive)
	source, err := ExtensionSourceURL(server.URL + "/cloudfile-updates/update.json")
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "extension-cache.json")
	first, err := fetchExtensionCachedAt(source, ExtensionCacheTTL, cachePath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fetchExtensionCachedAt(source, ExtensionCacheTTL, cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != second.Version {
		t.Fatalf("cached manifest returned %q, want %q", second.Version, first.Version)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("source was contacted %d times, want 1 (second call must hit the cache)", got)
	}
}

func TestReadExtensionCacheRejectsOtherSource(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "extension-cache.json")
	payload, err := json.Marshal(cachedExtensionManifest{
		Source:    "http://a.example/extension-update.json",
		FetchedAt: time.Now().Unix(),
		Version:   "1.0.0",
		ZipURL:    "./a.zip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readExtensionCacheAt("http://b.example/extension-update.json", ExtensionCacheTTL, cachePath); ok {
		t.Fatal("a cache entry from another source must be ignored")
	}
}
