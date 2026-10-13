// Package home locates the one per-machine CloudFile folder that the Windows
// installer and the local agent have to agree on.
//
// Everything stored per machine lives under it: the agent binaries, the
// unpacked browser extension, the native-host manifest, the download caches and
// the local mirror ({home}/sessions). The installer picks the folder
// (D:\CloudFileLocal, else C:\CloudFileLocal) and registers the manifest that
// lives there; the agent then follows whichever manifest is registered. Because
// both sides derive every path from this one answer, a self-update can never
// write into a folder the browser or Chrome does not read.
package home

import (
	"os"
	"path/filepath"
	"strings"
)

// ManifestName is the native-host manifest that anchors an installation. The
// Chrome registry key points at exactly one such file, so its folder is the
// install the running agent belongs to - not a guess about which drive looks
// nicer.
const ManifestName = "com.cloudfile.local_agent.json"

// envOverride pins the folder explicitly. It exists for tests and for an
// operator who wants the data somewhere else without rebuilding anything.
const envOverride = "CLOUDFILE_HOME"

// Dir returns the install folder, creating it when it has just been chosen.
//
// Lookup order:
//
//  1. CLOUDFILE_HOME.
//  2. the folder that already holds the native-host manifest. An existing
//     installation is never abandoned just because this version prefers a
//     different drive - the registry still points at the old one.
//  3. D:\CloudFileLocal then C:\CloudFileLocal, on a fixed, writable volume.
//  4. %LocalAppData%\CloudFileLocal, so a locked-down machine still works.
func Dir() string {
	return selectDir(
		strings.TrimSpace(os.Getenv(envOverride)),
		installRoots(),
		fallbackDir(),
		hasManifest,
		usableInstall,
	)
}

// selectDir is the whole policy as a pure function, so candidate order can be
// pinned by tests without touching real drives.
func selectDir(override string, roots []string, fallback string, anchored func(string) bool, usable func(string) bool) string {
	if override != "" {
		return filepath.Clean(override)
	}
	// The manifest wins over the drive preference: it is the location Chrome
	// was registered against.
	candidates := append(append([]string(nil), roots...), fallback)
	for _, base := range candidates {
		if anchored(base) {
			return base
		}
	}
	for _, base := range roots {
		if usable(base) {
			return base
		}
	}
	return fallback
}

func hasManifest(base string) bool {
	_, err := os.Stat(filepath.Join(base, ManifestName))
	return err == nil
}

// usableInstall reports whether a folder can hold an installation: a fixed
// volume (an optical or mapped D: must not win) that can actually be written to.
func usableInstall(base string) bool {
	return FixedDrive(base) && Writable(base)
}

// Writable creates the folder and probes it with a real file. A read-only mount
// or a full volume would otherwise only fail later, halfway through a download.
func Writable(path string) bool {
	if err := os.MkdirAll(path, 0700); err != nil {
		return false
	}
	probe, err := os.CreateTemp(path, ".write-probe-")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// fallbackDir is the per-user location, used when no fixed data volume is
// usable and by every non-Windows build.
func fallbackDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "CloudFileLocal"
	}
	return filepath.Join(dir, "CloudFileLocal")
}
