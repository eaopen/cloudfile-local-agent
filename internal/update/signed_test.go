package update

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestSignedReleaseBoundaries(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	public := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	now := time.Now()
	valid := SignedRelease{Version: "2.0.2", Platform: "darwin-arm64", SHA256: strings.Repeat("a", 64), DownloadURL: "https://updates.example/agent", ExpiresAt: now.Add(time.Hour).Unix(), IPCVersion: 1, JournalVersion: 1}
	sign := func(release SignedRelease) []byte {
		payload, _ := json.Marshal(release)
		digest := sha256.Sum256(payload)
		r, s, _ := ecdsa.Sign(rand.Reader, key, digest[:])
		raw := make([]byte, 64)
		r.FillBytes(raw[:32])
		s.FillBytes(raw[32:])
		data, _ := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(payload), "signature": base64.StdEncoding.EncodeToString(raw)})
		return data
	}
	data := sign(valid)
	if _, err := VerifyRelease(data, public, "https://updates.example/release.json", "darwin-arm64", now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*SignedRelease){
		"expired":       func(r *SignedRelease) { r.ExpiresAt = now.Unix() },
		"wrong_arch":    func(r *SignedRelease) { r.Platform = "darwin-amd64" },
		"wrong_ipc":     func(r *SignedRelease) { r.IPCVersion = 2 },
		"wrong_journal": func(r *SignedRelease) { r.JournalVersion = 2 },
		"cross_origin":  func(r *SignedRelease) { r.DownloadURL = "https://other.example/a" },
		"insecure":      func(r *SignedRelease) { r.DownloadURL = "http://updates.example/a" },
		"bad_version":   func(r *SignedRelease) { r.Version = "../../2.0.2" },
		"bad_hash":      func(r *SignedRelease) { r.SHA256 = "bad" },
		"extension_origin": func(r *SignedRelease) {
			r.Extension = &ExtensionManifest{Version: "1.0.2", ZIPURL: "https://other.example/a", SHA256: strings.Repeat("b", 64)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			mutate(&bad)
			if _, err := VerifyRelease(sign(bad), public, "https://updates.example/release.json", "darwin-arm64", now); err == nil {
				t.Fatal("accepted invalid release")
			}
		})
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherDER, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	wrong := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: otherDER}))
	if _, err := VerifyRelease(data, wrong, "https://updates.example/release.json", "darwin-arm64", now); err == nil {
		t.Fatal("accepted wrong signing key")
	}
	if _, err := VerifyRelease(data, "", "https://updates.example/release.json", "darwin-arm64", now); err == nil {
		t.Fatal("accepted unpinned release")
	}
}
