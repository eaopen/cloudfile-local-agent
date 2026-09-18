package update

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.3.0", 0},
		{"0.3.1", "0.3.0", 1},
		{"0.3.0", "0.4.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.9.9", 1},
		{"v1.2.3", "1.2.3", 0},
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if err != nil {
			t.Fatalf("Compare(%q, %q): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareRejectsMalformed(t *testing.T) {
	for _, value := range []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "1.2.x"} {
		if _, err := Compare(value, "0.0.0"); err == nil {
			t.Fatalf("Compare(%q) should fail", value)
		}
	}
}

func TestResolveDownloadURLRelativeToSource(t *testing.T) {
	got, err := resolveDownloadURL("http://host:6111/cloudfile-updates/update.json", "./cloudfile-local-agent-0.4.0.exe")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://host:6111/cloudfile-updates/cloudfile-local-agent-0.4.0.exe"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFetchRejectsIncompleteManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.4.0"}`))
	}))
	defer server.Close()
	if _, err := Fetch(server.URL); err == nil {
		t.Fatal("expected incomplete manifest to be rejected")
	}
}

func TestDownloadBinaryVerifiesChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("binary payload"))
	}))
	defer server.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "agent.exe")
	// Correct checksum succeeds.
	if err := downloadBinary(server.URL, target, "a2c31a6de3b1d6ee4e888e4c7d2a4e0b56b4dc35a9f8e2f8c8e6d5a4a3e2f1c0"); err == nil {
		t.Fatal("expected checksum mismatch for a wrong hash")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target must not exist after checksum failure")
	}
}

func TestDownloadBinaryWritesVerifiedFile(t *testing.T) {
	const payload = "verified\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "agent.exe")
	if err := downloadBinary(server.URL, target, sha256Hex(payload)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != payload {
		t.Fatalf("downloaded %q, err=%v", data, err)
	}
}

func TestPruneOldKeepsNewestTwo(t *testing.T) {
	dir := t.TempDir()
	for _, version := range []string{"0.1.0", "0.2.0", "0.3.0", "0.4.0"} {
		name := versionedName(version)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneOld(dir); err != nil {
		t.Fatal(err)
	}
	remaining := listBinaries(t, dir)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 kept binaries, got %v", remaining)
	}
	sort.Strings(remaining)
	if remaining[0] != "0.3.0" || remaining[1] != "0.4.0" {
		t.Fatalf("expected 0.3.0 and 0.4.0 kept, got %v", remaining)
	}
}

func listBinaries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cloudfile-local-agent-") {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "cloudfile-local-agent-"), ".exe"))
	}
	return out
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
