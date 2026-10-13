package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/home"
)

// manifestTimeout bounds a manifest fetch on the status/check_update path. The
// extension polls the agent every few hours; the answer has to come back
// quickly even when the update source is unreachable, so this stays well below
// the timeout of an explicit --check-update.
const manifestTimeout = 4 * time.Second

// CacheTTL is how long a fetched manifest is reused. Without it every poll
// would turn into a live request, and a machine on a slow or unreachable
// network would stall the Native Messaging call on each one.
const CacheTTL = 6 * time.Hour

type cachedManifest struct {
	Source      string `json:"source"`
	FetchedAt   int64  `json:"fetched_at"`
	Version     string `json:"version"`
	SHA256      string `json:"sha256,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

func (c cachedManifest) manifest() *Manifest {
	return &Manifest{
		Version:     c.Version,
		SHA256:      c.SHA256,
		DownloadURL: c.DownloadURL,
		Notes:       c.Notes,
	}
}

// CachePath is where the last manifest fetch is remembered, next to the
// native-host manifest and the versioned binaries (all under the shared
// install folder).
func CachePath() (string, error) {
	return filepath.Join(home.Dir(), "update-cache.json"), nil
}

// FetchCached returns the update manifest, reusing a cached copy that is
// younger than ttl and was fetched from the same source. A failed fetch is
// reported as an error instead of falling back to a stale cache, so a machine
// that cannot reach the source never advertises an update it cannot install.
func FetchCached(source string, ttl time.Duration) (*Manifest, error) {
	path, err := CachePath()
	if err != nil {
		// No writable cache dir: fall back to a plain fetch, still bounded.
		return fetchWithTimeout(source, manifestTimeout)
	}
	return fetchCachedAt(source, ttl, path)
}

// fetchCachedAt is FetchCached with an injectable cache path, used by tests.
func fetchCachedAt(source string, ttl time.Duration, path string) (*Manifest, error) {
	if cached, ok := readCacheAt(source, ttl, path); ok {
		return cached, nil
	}
	manifest, err := fetchWithTimeout(source, manifestTimeout)
	if err != nil {
		return nil, err
	}
	writeCacheAt(path, source, manifest)
	return manifest, nil
}

func readCacheAt(source string, ttl time.Duration, path string) (*Manifest, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cached cachedManifest
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	// A different source invalidates the cache: the operator may have repointed
	// update.json, and the old manifest must not shadow the new one.
	if cached.Source != source || cached.Version == "" {
		return nil, false
	}
	age := time.Since(time.Unix(cached.FetchedAt, 0))
	if age < 0 || age > ttl {
		return nil, false
	}
	return cached.manifest(), true
}

// writeCacheAt is best-effort: the cache only saves network round-trips, so a
// failure to persist it never fails the caller.
func writeCacheAt(path, source string, manifest *Manifest) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, err := json.MarshalIndent(cachedManifest{
		Source:      source,
		FetchedAt:   time.Now().Unix(),
		Version:     manifest.Version,
		SHA256:      manifest.SHA256,
		DownloadURL: manifest.DownloadURL,
		Notes:       manifest.Notes,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0600)
}
