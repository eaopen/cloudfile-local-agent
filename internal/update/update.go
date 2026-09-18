// Package update implements the CloudFile Local Agent self-update flow.
//
// The update source is an independent static file service (agreed with the
// user; deliberately NOT the Hub backend, to avoid coupling agent releases to
// a container rebuild). The source serves a single update.json plus the
// versioned binaries referenced by it.
//
// Security note: update.json (containing the SHA-256) and the binaries are
// served from the same static host, so the checksum only guards against
// transmission corruption — not against an attacker who can rewrite both.
// Tamper-resistance needs Authenticode signing, which is a follow-up item.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxBinaryBytes caps the downloaded update binary. The agent itself is ~6.4
// MiB; 256 MiB leaves ample headroom while bounding a rogue/misconfigured
// source from exhausting disk.
const maxBinaryBytes = 256 << 20

// Manifest mirrors update.json served by the static update source.
type Manifest struct {
	Version     string `json:"version"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
	MinVersion  string `json:"min_version"`
	Notes       string `json:"notes"`
}

// Fetch retrieves and decodes update.json from the source URL.
func Fetch(source string) (*Manifest, error) {
	response, err := (&http.Client{Timeout: 30 * time.Second}).Get(source)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update manifest unavailable (%d)", response.StatusCode)
	}
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode update manifest: %w", err)
	}
	if manifest.Version == "" || manifest.SHA256 == "" || manifest.DownloadURL == "" {
		return nil, fmt.Errorf("update manifest is incomplete (version/sha256/download_url required)")
	}
	return &manifest, nil
}

// Compare orders two dotted-triple versions (x.y.z). It returns -1, 0 or 1.
func Compare(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(value string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("invalid version %q", value)
	}
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return out, fmt.Errorf("invalid version %q", value)
		}
		out[i] = number
	}
	return out, nil
}

// Apply downloads and verifies the update, swaps the native-host manifest to
// point at the new binary, then lazily prunes old versions. It returns the
// path of the newly installed binary.
func Apply(source string, manifest *Manifest) (string, error) {
	binDir, err := BinDir()
	if err != nil {
		return "", err
	}
	manifestPath, err := ManifestPath()
	if err != nil {
		return "", err
	}
	return applyWithPaths(source, manifest, binDir, manifestPath)
}

// applyWithPaths is Apply with injectable paths, used by tests.
func applyWithPaths(source string, manifest *Manifest, binDir, manifestPath string) (string, error) {
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return "", err
	}
	downloadURL, err := resolveDownloadURL(source, manifest.DownloadURL)
	if err != nil {
		return "", err
	}
	target := filepath.Join(binDir, versionedName(manifest.Version))
	if err := downloadBinary(downloadURL, target, manifest.SHA256); err != nil {
		return "", err
	}
	if err := swapManifest(manifestPath, target); err != nil {
		return "", err
	}
	// Pruning failure is non-fatal: stale versions are retried on the next run.
	_ = pruneOld(binDir)
	return target, nil
}

// BinDir is where versioned agent binaries live, next to the native-host
// manifest and the session workspace (all under the per-user cache dir, which
// on Windows is %LocalAppData%).
func BinDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "CloudFileLocal", "bin"), nil
}

// ManifestPath mirrors register-windows.ps1, which writes the native-host
// manifest to %LocalAppData%\CloudFileLocal\com.cloudfile.local_agent.json
// (os.UserCacheDir on Windows). macOS/Linux native-host registration would
// need its own location; the current deployment is Windows-only.
func ManifestPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "CloudFileLocal", "com.cloudfile.local_agent.json"), nil
}

func versionedName(version string) string {
	name := "cloudfile-local-agent-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func resolveDownloadURL(source, downloadURL string) (string, error) {
	base, err := url.Parse(source)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(downloadURL)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func downloadBinary(rawURL, target, wantSHA256 string) error {
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Get(rawURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("binary download failed (%d)", response.StatusCode)
	}
	hasher := sha256.New()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".update-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(response.Body, maxBinaryBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxBinaryBytes {
		return fmt.Errorf("update binary is too large")
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("update binary checksum mismatch")
	}
	return os.Rename(tmpName, target)
}

// swapManifest rewrites only the "path" field of the native-host manifest,
// preserving name/description/type/allowed_origins. Writing the manifest (a
// plain JSON file) avoids overwriting a running executable, which Windows
// locks while the native host loop is alive.
func swapManifest(manifestPath, newBinary string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode native-host manifest: %w", err)
	}
	manifest["path"] = newBinary
	out, err := json.MarshalIndent(manifest, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, out, 0600)
}

// pruneOld keeps the two newest versioned binaries (current + one rollback
// point) and removes anything older. A still-running old binary may be locked
// by Chrome's native-host process; os.Remove failure is ignored and retried on
// the next invocation, so cleanup converges without timers or RunOnce.
func pruneOld(binDir string) error {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return err
	}
	var versions []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "cloudfile-local-agent-") {
			continue
		}
		version := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "cloudfile-local-agent-"), ".exe")
		if _, err := parseVersion(version); err != nil {
			continue
		}
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		comparison, err := Compare(versions[i], versions[j])
		return err == nil && comparison > 0
	})
	const keep = 2
	for i := keep; i < len(versions); i++ {
		_ = os.Remove(filepath.Join(binDir, versionedName(versions[i])))
	}
	return nil
}
