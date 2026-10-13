package editing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestCanonicalGoCommitDeviceSignature(t *testing.T) {
	now := time.Unix(1791735000, 0)
	c := DeviceCommitChallenge{Instance: "https://etech.example", DeviceID: "11111111-1111-4111-8111-111111111111",
		SessionID: "22222222-2222-4222-8222-222222222222", Operation: "commit",
		Nonce: base64.RawURLEncoding.EncodeToString(bytesOfLength(32)), IssuedAt: now.Unix() - 1,
		ExpiresAt: now.Unix() + 59, RequestSHA256: strings.Repeat("a", 64)}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignWithP256(key, c, c.Instance, c.DeviceID, c.SessionID, now)
	if err != nil || !VerifyP256Signature(&key.PublicKey, c, sig) {
		t.Fatalf("signature rejected: %v", err)
	}
	message, err := c.Message()
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := `cloudfile.device-proof.v1
{"device_id":"11111111-1111-4111-8111-111111111111","expires_at":1791735059,"instance":"https://etech.example","issued_at":1791734999,"nonce":"`
	if !strings.HasPrefix(string(message), wantPrefix) {
		t.Errorf("noncanonical proof message: %s", message)
	}
	// Independently computed with Hub/Python DeviceChallenge.message().
	wantDigest := "151b7a256dcd805d360846444510f2be2a12862de7c092bd093ca62f63d1e4e2"
	got := sha256.Sum256(message)
	if hex.EncodeToString(got[:]) != wantDigest {
		t.Fatalf("Go and Hub canonical P-256 proof messages differ: %x", got)
	}
	mutated := c
	mutated.RequestSHA256 = strings.Repeat("b", 64)
	if VerifyP256Signature(&key.PublicKey, mutated, sig) {
		t.Fatal("signature was transferable to another commit")
	}
	mutated = c
	mutated.Operation = "read"
	if _, err := SignWithP256(key, mutated, c.Instance, c.DeviceID, c.SessionID, now); err == nil {
		t.Fatal("signed wrong operation")
	}
	if _, err := SignWithP256(key, c, c.Instance, c.DeviceID, c.SessionID, now.Add(120*time.Second)); err == nil {
		t.Fatal("signed expired challenge")
	}
	if _, err := SignWithP256(key, c, "https://other.example", c.DeviceID, c.SessionID, now); err == nil {
		t.Fatal("signed other origin")
	}
}
func bytesOfLength(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
