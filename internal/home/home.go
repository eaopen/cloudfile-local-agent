// Package home locates the per-user portable CloudFile installation.
// It never installs a privileged service.
package home

import (
	"os"
	"path/filepath"
	"strings"
)

const ManifestName = "com.cloudfile.agent2.json"

// resolveInstalledHome accepts both layouts used by the 1.0.x installer:
// a versioned EXE in <root> and a self-updated EXE in <root>/bin.
// The native host manifest is the authority; only an existing manifest is
// followed, never a path suggested by the remote server.
func resolveInstalledHome(executable string) string {
	dir := filepath.Dir(executable)
	candidates := []string{dir}
	if strings.EqualFold(filepath.Base(dir), "bin") {
		candidates = append(candidates, filepath.Dir(dir))
	}
	for _, root := range candidates {
		info, err := os.Stat(filepath.Join(root, ManifestName))
		if err == nil && info.Mode().IsRegular() {
			return root
		}
	}
	return ""
}
func Dir() string {
	executable, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		if dir := resolveInstalledHome(executable); dir != "" {
			return dir
		}
	}
	// Fallback for an unpacked/unregistered release. A normal user may use
	// %APPDATA% on Windows and XDG_CONFIG_HOME on Unix without elevation.
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "CloudFileAgent2")
	}
	return filepath.Join(base, "CloudFileAgent2")
}
