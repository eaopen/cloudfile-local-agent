package update

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// manifestServer serves a fixed update.json and counts how many times it was
// actually hit, so cache behaviour is observable (hit count, not timing).
func manifestServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"version": "0.9.9",
			"sha256": "abc",
			"download_url": "cloudfile-local-agent-0.9.9.exe",
			"notes": "test"
		}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchCachedReusesFreshCache(t *testing.T) {
	var hits int32
	server := manifestServer(t, &hits)
	cachePath := filepath.Join(t.TempDir(), "update-cache.json")

	for i := 0; i < 3; i++ {
		manifest, err := fetchCachedAt(server.URL, CacheTTL, cachePath)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if manifest.Version != "0.9.9" || manifest.Notes != "test" {
			t.Fatalf("call %d: unexpected manifest %+v", i+1, manifest)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("source hit %d times, want 1 (cache should serve the rest)", got)
	}
}

func TestFetchCachedRefetchesWhenExpired(t *testing.T) {
	var hits int32
	server := manifestServer(t, &hits)
	cachePath := filepath.Join(t.TempDir(), "update-cache.json")

	if _, err := fetchCachedAt(server.URL, CacheTTL, cachePath); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// ttl 0 means any entry is already stale.
	if _, err := fetchCachedAt(server.URL, 0, cachePath); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("source hit %d times, want 2", got)
	}
}

func TestFetchCachedIgnoresCacheFromAnotherSource(t *testing.T) {
	var hits int32
	server := manifestServer(t, &hits)
	cachePath := filepath.Join(t.TempDir(), "update-cache.json")

	if _, err := fetchCachedAt(server.URL, CacheTTL, cachePath); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := fetchCachedAt(server.URL+"/other/update.json", CacheTTL, cachePath); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("source hit %d times, want 2 (repointed source must refetch)", got)
	}
}

func TestFetchCachedReportsUnreachableSource(t *testing.T) {
	server := manifestServer(t, new(int32))
	url := server.URL
	server.Close() // now unreachable

	cachePath := filepath.Join(t.TempDir(), "update-cache.json")
	if _, err := fetchCachedAt(url, CacheTTL, cachePath); err == nil {
		t.Fatal("want an error when the source is unreachable and no cache exists")
	}
}

func TestFetchCachedDoesNotServeStaleCacheOnFailure(t *testing.T) {
	var hits int32
	server := manifestServer(t, &hits)
	cachePath := filepath.Join(t.TempDir(), "update-cache.json")

	if _, err := fetchCachedAt(server.URL, CacheTTL, cachePath); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	url := server.URL
	server.Close()

	// Expired cache + dead source must be an error, not an old manifest.
	if _, err := fetchCachedAt(url, 0, cachePath); err == nil {
		t.Fatal("want an error rather than a stale manifest")
	}
}

func TestReadCacheAtRejectsIncompleteEntry(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-cache.json")
	body := `{"source":"x","fetched_at":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"version":""}`
	if err := os.WriteFile(cachePath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCacheAt("x", CacheTTL, cachePath); ok {
		t.Fatal("an entry without a version must be treated as a miss")
	}
}
