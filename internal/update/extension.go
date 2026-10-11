package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/eaopen/cloudfile-local-agent/internal/home"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cached version probes must not hold up Native Messaging.
const CacheTTL = time.Minute * 3
const ExtensionCacheTTL = time.Minute * 3

type ExtensionManifest struct {
	Version string `json:"version"`
	ZIPURL  string `json:"zip_url"`
	SHA256  string `json:"sha256"`
	Notes   string `json:"notes"`
}

type cachedAgent struct {
	manifest *Manifest
	expires  time.Time
}
type cachedExtension struct {
	manifest *ExtensionManifest
	expires  time.Time
}

var cacheLock sync.Mutex
var cacheAgents = map[string]cachedAgent{}
var cacheExtensions = map[string]cachedExtension{}

// FetchCached never writes an update; fetching a version is informational.
func FetchCached(source string, ttl time.Duration) (*Manifest, error) {
	if source == "" {
		return nil, errors.New("update source missing")
	}
	cacheLock.Lock()
	entry, exists := cacheAgents[source]
	if exists && time.Now().Before(entry.expires) {
		cacheLock.Unlock()
		return entry.manifest, nil
	}
	cacheLock.Unlock()
	manifest, err := fetchWithTimeout(source, 2*time.Second)
	if err != nil {
		return nil, err
	}
	cacheLock.Lock()
	cacheAgents[source] = cachedAgent{manifest: manifest, expires: time.Now().Add(ttl)}
	cacheLock.Unlock()
	return manifest, nil
}

func ExtensionSourceURL(agentSource string) (string, error) {
	u, err := url.Parse(agentSource)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("invalid update source")
	}
	sibling, err := url.Parse("extension-update.json")
	if err != nil {
		return "", err
	}
	return u.ResolveReference(sibling).String(), nil
}

func FetchExtension(source string) (*ExtensionManifest, error) {
	c := http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(source)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("extension manifest HTTP %d", resp.StatusCode)
	}
	var m ExtensionManifest
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m)
	if err != nil {
		return nil, err
	}
	if m.Version == "" || m.ZIPURL == "" || m.SHA256 == "" || len(m.SHA256) != 64 {
		return nil, errors.New("incomplete extension manifest")
	}
	return &m, nil
}
func FetchExtensionCached(agentSource string, ttl time.Duration) (*ExtensionManifest, error) {
	source, err := ExtensionSourceURL(agentSource)
	if err != nil {
		return nil, err
	}
	cacheLock.Lock()
	entry, ok := cacheExtensions[source]
	if ok && time.Now().Before(entry.expires) {
		cacheLock.Unlock()
		return entry.manifest, nil
	}
	cacheLock.Unlock()
	m, err := FetchExtension(source)
	if err != nil {
		return nil, err
	}
	cacheLock.Lock()
	cacheExtensions[source] = cachedExtension{manifest: m, expires: time.Now().Add(ttl)}
	cacheLock.Unlock()
	return m, nil
}

func ExtensionDir() (string, error) {
	return filepath.Join(home.Dir(), "extension"), nil
}
func InstalledExtensionVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if _, err := parseVersion(m.Version); err != nil {
		return ""
	}
	return m.Version
}

// ApplyExtension downloads an archive to a separate temp directory and
// installs it without touching the running agent binary. Do not follow
// symlinks or allow ZIP paths to escape the unpacked extension root.
func ApplyExtension(source string, m *ExtensionManifest, dir string) (string, error) {
	if m == nil || m.Version == "" || m.ZIPURL == "" || len(m.SHA256) != 64 || dir == "" {
		return "", errors.New("verified update manifest required")
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", err
	}
	destination, err := u.Parse(m.ZIPURL)
	if err != nil {
		return "", err
	}
	target := u.ResolveReference(destination)
	if target.Scheme != u.Scheme || target.Host != u.Host || target.User != nil || target.Fragment != "" {
		return "", errors.New("same-origin extension source required")
	}
	tempParent := filepath.Dir(dir)
	if err := os.MkdirAll(tempParent, 0700); err != nil {
		return "", err
	}
	zipPathFile, err := os.CreateTemp(tempParent, ".cf-extension-*.zip")
	if err != nil {
		return "", err
	}
	zipPath := zipPathFile.Name()
	zipPathFile.Close()
	if err := os.Remove(zipPath); err != nil {
		return "", err
	}
	defer os.Remove(zipPath)
	if err := downloadBinary(target.String(), zipPath, m.SHA256); err != nil {
		return "", err
	}
	unpack, err := os.MkdirTemp(tempParent, ".cf-extension-new-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(unpack)
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	var total int64
	for _, item := range archive.File {
		name := filepath.Clean(item.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || strings.Contains(name, "\\") || item.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("invalid extension archive path")
		}
		path := filepath.Join(unpack, name)
		if item.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0700); err != nil {
				return "", err
			}
			continue
		}
		if !item.Mode().IsRegular() {
			return "", errors.New("invalid extension archive member")
		}
		total += int64(item.UncompressedSize64)
		if total > 256<<20 {
			return "", errors.New("extension archive exceeds limit")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		source, err := item.Open()
		if err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			source.Close()
			return "", err
		}
		copied, err := io.CopyN(f, source, int64(item.UncompressedSize64)+1)
		source.Close()
		closeErr := f.Close()
		if err != nil && err != io.EOF {
			return "", err
		}
		if closeErr != nil || copied != int64(item.UncompressedSize64) {
			return "", errors.New("extension archive file length mismatch")
		}
	}
	if InstalledExtensionVersion(unpack) != m.Version {
		return "", errors.New("extension archive version mismatch")
	}
	backup := dir + ".prior"
	if _, err := os.Stat(backup); err == nil {
		return "", errors.New("previous extension backup still present")
	}
	hadOld := false
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, backup); err != nil {
			return "", err
		}
		hadOld = true
	}
	if err := os.Rename(unpack, dir); err != nil {
		if hadOld {
			_ = os.Rename(backup, dir)
		}
		return "", err
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}
	return dir, nil
}

// Keep package-level SHA-256 reuse explicit for manifest review.
var _ = sha256.Size
var _ = hex.EncodeToString
