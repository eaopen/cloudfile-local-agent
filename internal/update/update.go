// Package update implements the CloudFile Local Agent self-update flow.
//
// The update source is an independent static file service (agreed with the
// user; deliberately NOT the Hub backend, to avoid coupling agent releases to
// a container rebuild). The source serves a single update.json plus the
// versioned binaries referenced by it.
//
// macOS uses signed.go with a locally pinned public key. The legacy Fetch
// decoder remains for compatibility on other Unix platforms; Windows product
// updates are controlled by the .NET signed bundle updater.
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
)

// maxBinaryBytes caps the downloaded update binary. The agent itself is ~6.4
// MiB; 256 MiB leaves ample headroom while bounding a rogue/misconfigured
// source from exhausting disk.
const maxBinaryBytes = 256 << 20

// Manifest mirrors update.json served by the static update source.
//
// There is deliberately no min_version field: an update is only ever a prompt.
// (The release JSONs may still carry the key; unknown keys are ignored.)
type Manifest struct {
	expiresAt   int64
	Version     string             `json:"version"`
	SHA256      string             `json:"sha256"`
	DownloadURL string             `json:"download_url"`
	Notes       string             `json:"notes"`
	Extension   *ExtensionManifest `json:"extension,omitempty"`
}

// Fetch retrieves and decodes update.json from the source URL. It keeps the
// long timeout because it backs an explicit user action (--check-update /
// --update); the lightweight status/check_update path uses FetchCached, which
// bounds the wait far more tightly.
func Fetch(source string) (*Manifest, error) {
	return fetchWithTimeout(source, 30*time.Second)
}

func fetchWithTimeout(source string, timeout time.Duration) (*Manifest, error) {
	response, err := (&http.Client{Timeout: timeout}).Get(source)
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
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return out, fmt.Errorf("invalid version %q", value)
		}
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
	lock := filepath.Join(filepath.Dir(binDir), ".install-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return "", fmt.Errorf("installation already active or unavailable")
	}
	defer os.Remove(lock)
	if runtime.GOOS == "darwin" {
		if manifest == nil || manifest.expiresAt <= time.Now().Unix() {
			return "", fmt.Errorf("current signed macOS release required")
		}
		previous, err := os.ReadFile(manifestPath)
		if err != nil {
			return "", err
		}
		var current struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Path string `json:"path"`
		}
		if json.Unmarshal(previous, &current) != nil || current.Name != "com.cloudfile.agent2" || current.Type != "stdio" || filepath.Dir(current.Path) != binDir {
			return "", fmt.Errorf("owned macOS registration required")
		}
		info, err := os.Lstat(current.Path)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("registered executable unavailable")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		active, err := candidateOutput(ctx, current.Path, nil, "--version")
		cancel()
		if err != nil {
			return "", err
		}
		comparison, err := Compare(manifest.Version, strings.TrimSpace(string(active)))
		if err != nil {
			return "", err
		}
		if comparison <= 0 {
			return current.Path, nil
		}
		if err := atomicManifest(filepath.Join(filepath.Dir(binDir), "previous-native-host.json"), previous); err != nil {
			return "", err
		}
	}
	return applyChecked(source, manifest, binDir, manifestPath, true)
}

// applyWithPaths is Apply with injectable paths, used by tests.
func applyWithPaths(source string, manifest *Manifest, binDir, manifestPath string) (string, error) {
	return applyChecked(source, manifest, binDir, manifestPath, false)
}

func applyChecked(source string, manifest *Manifest, binDir, manifestPath string, check bool) (string, error) {
	if manifest == nil {
		return "", fmt.Errorf("update manifest required")
	}
	if _, err := parseVersion(manifest.Version); err != nil {
		return "", err
	}
	// Validate the registration before downloading. Follow the installer's
	// symlink once, then atomically replace the actual browser manifest.
	resolved, err := filepath.EvalSymlinks(manifestPath)
	if err != nil {
		return "", err
	}
	manifestPath = resolved

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
	if check {
		if err := CheckExecutable(target, manifest.Version); err != nil {
			return "", err
		}
	}
	if err := swapManifest(manifestPath, target); err != nil {
		return "", err
	}
	// An editor may still be using an older executable. Retain old versions;
	// removal belongs to an explicit maintenance action, not activation.
	return target, nil
}

// BinDir is where versioned agent binaries live, next to the native-host
// manifest, the unpacked extension and the mirror - all under the shared
// install folder (see internal/home). The installer creates that folder and the
// agent follows it, so a self-update always lands beside the manifest that
// Chrome was registered against.
func BinDir() (string, error) {
	return filepath.Join(home.Dir(), "bin"), nil
}

// ManifestPath is the native-host manifest inside the shared install folder;
// register-windows.ps1 writes exactly this file and points the Chrome registry
// key at it, so it is the anchor home.Dir() follows.
func ManifestPath() (string, error) {
	return filepath.Join(home.Dir(), home.ManifestName), nil
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
	response, err := (&http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(rawURL)
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
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if syncErr != nil {
		return syncErr
	}
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
	if err := os.Chmod(tmpName, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("regular target required")
	}
	if existing, err := os.ReadFile(target); err == nil {
		digest := sha256.Sum256(existing)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), wantSHA256) {
			return fmt.Errorf("installed version differs; refusing overwrite")
		}
		return os.Chmod(target, 0700)
	} else if !os.IsNotExist(err) {
		return err
	}
	// Hard-link installation fails if a concurrent installer claimed this name.
	return os.Link(tmpName, target)
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
	return atomicManifest(manifestPath, out)
}

func atomicManifest(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cloudfile-manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// CheckExecutable runs the candidate before changing the browser entry point.
func CheckExecutable(path, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := candidateOutput(ctx, path, nil, "--version")
	if err != nil || strings.TrimSpace(string(output)) != version {
		return fmt.Errorf("candidate executable health check failed")
	}
	if runtime.GOOS == "darwin" {
		output, err = candidateOutput(ctx, path, []byte(`{"ipc_version":1,"request":{"type":"capabilities"}}`), "--component")
		var response struct {
			OK     bool `json:"ok"`
			Result struct {
				Platform string `json:"platform"`
				IPC      int    `json:"ipc_version"`
				Journal  int    `json:"journal_version"`
			} `json:"result"`
		}
		if err != nil || json.Unmarshal(output, &response) != nil || !response.OK || response.Result.Platform != "darwin" || response.Result.IPC != 1 || response.Result.Journal != 1 {
			return fmt.Errorf("candidate protocol health check failed")
		}
	}
	return nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 16384 {
		return 0, fmt.Errorf("candidate output exceeds budget")
	}
	return b.Buffer.Write(data)
}
func candidateOutput(ctx context.Context, path string, input []byte, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = bytes.NewReader(input)
	var output boundedOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("candidate execution failed")
	}
	return output.Bytes(), nil
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
