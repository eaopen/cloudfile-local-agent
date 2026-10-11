package update

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// The public key is provisioned locally, never accepted from the release feed.
func PublicKey(value string) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("one P-256 public key required")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve.Params().Name != "P-256" {
		return nil, errors.New("P-256 public key required")
	}
	return key, nil
}

type SignedRelease struct {
	Version        string             `json:"version"`
	Platform       string             `json:"platform"`
	SHA256         string             `json:"sha256"`
	DownloadURL    string             `json:"download_url"`
	Notes          string             `json:"notes"`
	ExpiresAt      int64              `json:"expires_at"`
	IPCVersion     int                `json:"ipc_version"`
	JournalVersion int                `json:"journal_version"`
	Extension      *ExtensionManifest `json:"extension,omitempty"`
}

func sameHTTPSOrigin(source, download string) bool {
	a, err := url.Parse(source)
	if err != nil {
		return false
	}
	b, err := url.Parse(download)
	return err == nil && a.Scheme == "https" && b.Scheme == "https" && a.Host != "" &&
		strings.EqualFold(a.Host, b.Host) && a.User == nil && b.User == nil && a.Fragment == "" && b.Fragment == ""
}

func VerifyRelease(data []byte, keyPEM, source, platform string, now time.Time) (*Manifest, error) {
	key, err := PublicKey(keyPEM)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if len(data) > 65536 || json.Unmarshal(data, &envelope) != nil {
		return nil, errors.New("invalid signed release")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, errors.New("invalid release payload")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != 64 {
		return nil, errors.New("invalid release signature")
	}
	digest := sha256.Sum256(payload)
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return nil, errors.New("release signature rejected")
	}
	var release SignedRelease
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&release) != nil || decoder.Decode(new(any)) != io.EOF ||
		release.Platform != platform || release.ExpiresAt <= now.Unix() || release.IPCVersion != 1 || release.JournalVersion != 1 ||
		!validHash(release.SHA256) || !sameHTTPSOrigin(source, release.DownloadURL) {
		return nil, errors.New("release platform, expiry, compatibility or download rejected")
	}
	if _, err := parseVersion(release.Version); err != nil {
		return nil, err
	}
	if e := release.Extension; e != nil {
		if _, err := parseVersion(e.Version); err != nil {
			return nil, err
		}
		if !validHash(e.SHA256) || !sameHTTPSOrigin(source, e.ZIPURL) {
			return nil, errors.New("extension artifact rejected")
		}
	}
	return &Manifest{expiresAt: release.ExpiresAt, Version: release.Version, SHA256: release.SHA256, DownloadURL: release.DownloadURL, Notes: release.Notes, Extension: release.Extension}, nil
}

func validHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func FetchSigned(source, keyPEM string, timeout time.Duration) (*Manifest, error) {
	if _, err := PublicKey(keyPEM); err != nil {
		return nil, err
	}
	if !sameHTTPSOrigin(source, source) {
		return nil, errors.New("HTTPS release source required")
	}
	cacheKey := "signed:" + source + "\n" + keyPEM
	if timeout <= 2*time.Second {
		cacheLock.Lock()
		entry, exists := cacheAgents[cacheKey]
		cacheLock.Unlock()
		if exists && time.Now().Before(entry.expires) && time.Now().Unix() < entry.manifest.expiresAt {
			return entry.manifest, nil
		}
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(source)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("signed release unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return nil, err
	}
	manifest, err := VerifyRelease(data, keyPEM, source, runtime.GOOS+"-"+runtime.GOARCH, time.Now())
	if err != nil {
		return nil, err
	}
	cacheLock.Lock()
	cacheAgents[cacheKey] = cachedAgent{manifest: manifest, expires: time.Now().Add(CacheTTL)}
	cacheLock.Unlock()
	return manifest, nil
}
