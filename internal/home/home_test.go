package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortableAgentHomeFollowsRegisteredManifest(t *testing.T) {
	root := t.TempDir()
	if got := resolveInstalledHome(filepath.Join(root, "cloudfile-local-agent.exe")); got != "" {
		t.Fatalf("unregistered home was trusted: %s", got)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveInstalledHome(filepath.Join(root, "cloudfile-local-agent.exe")); got != root {
		t.Fatalf("root install home=%q", got)
	}
	if got := resolveInstalledHome(filepath.Join(root, "bin", "cloudfile-local-agent-1.0.1.exe")); got != root {
		t.Fatalf("versioned update home=%q", got)
	}
}
