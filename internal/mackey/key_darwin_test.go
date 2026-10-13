//go:build darwin && cgo

package mackey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"testing"
)

func TestInvalidAlias(t *testing.T) {
	if _, err := Open("../../existing"); err == nil {
		t.Fatal("invalid alias accepted")
	}
}

// Explicit opt-in: creates and removes a unique temporary device key only.
func TestKeychainRoundTrip(t *testing.T) {
	if os.Getenv("CLOUDFILE_TEST_KEYCHAIN") != "1" {
		t.Skip("live user Keychain integration requires explicit opt-in")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	alias := fmt.Sprintf("%x-%x-%x-%x-%x", random[:4], random[4:6], random[6:8], random[8:10], random[10:])
	defer func() {
		if err := Delete(alias); err != nil {
			t.Error(err)
		}
	}()
	key, err := Create(alias)
	if err != nil {
		t.Fatal(err)
	}
	public, err := key.PublicJWK()
	if err != nil {
		key.Close()
		t.Fatal(err)
	}
	thumb, err := key.Thumbprint()
	if err != nil {
		key.Close()
		t.Fatal(err)
	}
	key.Close()
	reopened, err := Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Thumbprint()
	if err != nil || thumb != again {
		t.Fatal("persistent key identity changed")
	}
	digest := sha256.Sum256([]byte("CloudFile temporary Keychain integration test"))
	signature, err := reopened.SignDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(raw) != 64 {
		t.Fatal("signature format")
	}
	x, _ := base64.RawURLEncoding.DecodeString(public["x"])
	y, _ := base64.RawURLEncoding.DecodeString(public["y"])
	pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !ecdsa.Verify(&pub, digest[:], new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])) {
		t.Fatal("Keychain signature did not verify")
	}
}
