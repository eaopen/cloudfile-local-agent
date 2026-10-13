package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
)

// ExtensionManifest mirrors extension-update.json, which lives in the same
// static directory as update.json. Like the agent manifest it carries no
// min_version: an available version is a prompt, never a gate.
type ExtensionManifest struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	ZipURL  string `json:"zip_url"`
	Notes   string `json:"notes"`
}

const (
	// extensionManifestName is the fixed file name inside the update directory;
	// deriving it (instead of configuring a second URL) keeps the agent's config
	// to a single address and lets nginx move both files together.
	extensionManifestName = "extension-update.json"
	// maxExtensionZipBytes bounds the downloaded package. The real bundle is
	// well under 200 KiB; the headroom covers future icon assets while still
	// stopping a rogue source from filling the disk.
	maxExtensionZipBytes = 32 << 20
	// extensionManifestTimeout matches the agent manifest's: this fetch backs
	// the extension's background polling and must return quickly when the
	// source is unreachable.
	extensionManifestTimeout = 4 * time.Second
	// ExtensionCacheTTL is how long a fetched extension manifest is reused.
	ExtensionCacheTTL = 6 * time.Hour
)

// ExtensionSourceURL derives the extension manifest URL from the agent manifest
// URL. Both are served from the same directory, so the operator only ever
// configures one address.
func ExtensionSourceURL(updateSource string) (string, error) {
	parsed, err := url.Parse(updateSource)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("update source %q is not an absolute URL", updateSource)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	trimmed := strings.TrimSuffix(parsed.Path, "/")
	if trimmed == "" || !strings.HasSuffix(trimmed, ".json") {
		parsed.Path = trimmed + "/" + extensionManifestName
		return parsed.String(), nil
	}
	parsed.Path = path.Dir(trimmed) + "/" + extensionManifestName
	return parsed.String(), nil
}

// ExtensionDir is the directory Chrome loads the unpacked extension from.
// register-windows.ps1 extracts the release zip there, and the self-update
// writes back to the same place so a runtime reload picks up the new files.
func ExtensionDir() (string, error) {
	return filepath.Join(home.Dir(), "extension"), nil
}

func extensionCachePath() (string, error) {
	return filepath.Join(home.Dir(), "extension-cache.json"), nil
}

// FetchExtension retrieves extension-update.json with the long timeout used by
// explicit user actions (--update-extension).
func FetchExtension(source string) (*ExtensionManifest, error) {
	return fetchExtensionWithTimeout(source, 30*time.Second)
}

// FetchExtensionCached is the polling path: bounded timeout plus a short-lived
// local cache, so a background probe never stalls on an unreachable source.
func FetchExtensionCached(updateSource string, ttl time.Duration) (*ExtensionManifest, error) {
	source, err := ExtensionSourceURL(updateSource)
	if err != nil {
		return nil, err
	}
	cachePath, err := extensionCachePath()
	if err != nil {
		// No writable cache dir: still fetch, just without the cache.
		return fetchExtensionWithTimeout(source, extensionManifestTimeout)
	}
	return fetchExtensionCachedAt(source, ttl, cachePath)
}

func fetchExtensionCachedAt(source string, ttl time.Duration, cachePath string) (*ExtensionManifest, error) {
	if cached, ok := readExtensionCacheAt(source, ttl, cachePath); ok {
		return cached, nil
	}
	manifest, err := fetchExtensionWithTimeout(source, extensionManifestTimeout)
	if err != nil {
		return nil, err
	}
	writeExtensionCacheAt(cachePath, source, manifest)
	return manifest, nil
}

func fetchExtensionWithTimeout(source string, timeout time.Duration) (*ExtensionManifest, error) {
	response, err := (&http.Client{Timeout: timeout}).Get(source)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("extension update manifest unavailable (%d)", response.StatusCode)
	}
	var manifest ExtensionManifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode extension update manifest: %w", err)
	}
	if manifest.Version == "" || manifest.ZipURL == "" {
		return nil, fmt.Errorf("extension update manifest is incomplete (version/zip_url required)")
	}
	return &manifest, nil
}

type cachedExtensionManifest struct {
	Source    string `json:"source"`
	FetchedAt int64  `json:"fetched_at"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256,omitempty"`
	ZipURL    string `json:"zip_url,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

func (c cachedExtensionManifest) manifest() *ExtensionManifest {
	return &ExtensionManifest{
		Version: c.Version,
		SHA256:  c.SHA256,
		ZipURL:  c.ZipURL,
		Notes:   c.Notes,
	}
}

func readExtensionCacheAt(source string, ttl time.Duration, cachePath string) (*ExtensionManifest, bool) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	var cached cachedExtensionManifest
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	if cached.Source != source || cached.Version == "" || cached.ZipURL == "" {
		return nil, false
	}
	age := time.Since(time.Unix(cached.FetchedAt, 0))
	if age < 0 || age > ttl {
		return nil, false
	}
	return cached.manifest(), true
}

func writeExtensionCacheAt(cachePath, source string, manifest *ExtensionManifest) {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
		return
	}
	data, err := json.MarshalIndent(cachedExtensionManifest{
		Source:    source,
		FetchedAt: time.Now().Unix(),
		Version:   manifest.Version,
		SHA256:    manifest.SHA256,
		ZipURL:    manifest.ZipURL,
		Notes:     manifest.Notes,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(cachePath, append(data, '\n'), 0600)
}

// InstalledExtensionVersion reports the version currently sitting on disk in
// dir (empty when the directory holds no readable manifest). The extension
// compares it with its own loaded version to decide when a reload is safe.
func InstalledExtensionVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}
	return manifest.Version
}

// ApplyExtension downloads the extension package, verifies its SHA-256 and
// replaces the unpacked extension in dir. It returns dir.
func ApplyExtension(source string, manifest *ExtensionManifest, dir string) (string, error) {
	if manifest.SHA256 == "" {
		return "", fmt.Errorf("extension update manifest has no sha256; refusing to install an unverified package")
	}
	downloadURL, err := resolveDownloadURL(source, manifest.ZipURL)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	staging := dir + ".staging"
	_ = os.RemoveAll(staging)

	archive, err := os.CreateTemp(parent, ".extension-*.zip")
	if err != nil {
		return "", err
	}
	archivePath := archive.Name()
	archive.Close()
	defer os.Remove(archivePath)
	if err := downloadToFile(downloadURL, archivePath, manifest.SHA256, maxExtensionZipBytes); err != nil {
		return "", err
	}
	if err := extractZip(archivePath, staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	if _, err := os.Stat(filepath.Join(staging, "manifest.json")); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("extension package is incomplete (manifest.json missing)")
	}
	if err := swapDirectory(staging, dir); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return dir, nil
}

// swapDirectory puts staging in place of dir, keeping the previous contents
// only long enough to remove them. Windows refuses to rename a directory whose
// files are open, so a copy fallback covers the browser holding a handle on the
// old files.
func swapDirectory(staging, dir string) error {
	if _, err := os.Stat(dir); err != nil {
		return os.Rename(staging, dir)
	}
	previous := dir + ".previous"
	_ = os.RemoveAll(previous)
	if err := os.Rename(dir, previous); err == nil {
		if err := os.Rename(staging, dir); err == nil {
			_ = os.RemoveAll(previous)
			return nil
		}
		// Put the old directory back so the extension stays loadable.
		_ = os.Rename(previous, dir)
	}
	return copyTree(staging, dir)
}

func copyTree(from, to string) error {
	if err := os.MkdirAll(to, 0755); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, current)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		payload, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		return os.WriteFile(target, payload, 0644)
	})
}

// downloadToFile streams rawURL into target, verifying wantSHA256 and refusing
// payloads above limit bytes.
func downloadToFile(rawURL, target, wantSHA256 string, limit int64) error {
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Get(rawURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed (%d): %s", response.StatusCode, rawURL)
	}
	hasher := sha256.New()
	temporary, err := os.CreateTemp(filepath.Dir(target), ".download-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, err := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(response.Body, limit+1))
	closeErr := temporary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if written > limit {
		return fmt.Errorf("download is too large: %s", rawURL)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("checksum mismatch for %s", rawURL)
	}
	return os.Rename(temporaryPath, target)
}

// extractZip unpacks archive into dest, rejecting entries that would escape the
// destination (zip-slip) and capping the unpacked size.
func extractZip(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open extension package: %w", err)
	}
	defer reader.Close()
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	var total int64
	for _, file := range reader.File {
		name := strings.TrimPrefix(file.Name, "./")
		if name == "" {
			continue
		}
		if path.IsAbs(name) || strings.Contains(name, `\`) || strings.HasPrefix(name, "../") ||
			strings.Contains(name, "/../") || path.Clean(name) == ".." {
			return fmt.Errorf("extension package entry escapes the target directory: %q", file.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		total += int64(file.UncompressedSize64)
		if total > maxExtensionZipBytes {
			return fmt.Errorf("extension package is too large")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		payload, err := io.ReadAll(io.LimitReader(source, maxExtensionZipBytes))
		source.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, payload, 0644); err != nil {
			return err
		}
	}
	return nil
}
