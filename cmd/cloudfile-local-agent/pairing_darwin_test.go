//go:build darwin && cgo

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/mackey"
)

func TestMacPairingHandshake(t *testing.T) {
	if os.Getenv("CLOUDFILE_TEST_KEYCHAIN") != "1" {
		t.Skip("live Keychain integration opt-in")
	}

	var id [16]byte
	rand.Read(id[:])
	alias := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	defer func() {
		if err := mackey.Delete(alias); err != nil {
			t.Error(err)
		}
	}()
	key, err := mackey.Create(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	thumb, err := key.Thumbprint()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pending := macPairing{Instance: "https://cloud.example", Alias: alias, Thumbprint: thumb, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	path := filepath.Join(dir, "pending-"+alias+".json")
	if err := savePairing(path, pending); err != nil {
		t.Fatal(err)
	}
	call := func(action string, input any) ([]byte, error) {
		in, err := os.CreateTemp(t.TempDir(), "input")
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		json.NewEncoder(in).Encode(input)
		in.Seek(0, 0)
		out, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		oldIn, oldOut := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = in, out
		defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
		result := runPairingIn(dir, action, "", alias)
		data, _ := os.ReadFile(out.Name())
		return data, result
	}
	deviceID := "11111111-1111-4111-8111-111111111111"
	request, _ := json.Marshal(map[string]string{"device_id": deviceID, "pairing_id": deviceID, "key_thumbprint": thumb})
	digest := sha256.Sum256(request)
	challenge := device.Challenge{Instance: pending.Instance, DeviceID: deviceID, SessionID: deviceID, Operation: "pair", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), RequestSHA256: hex.EncodeToString(digest[:])}
	input := map[string]any{"device_id": deviceID, "state": "pending", "revision": "1", "key_thumbprint": thumb, "challenge": challenge}
	data, err := call("sign", input)
	if err != nil {
		t.Fatal(err)
	}
	var proof map[string]string
	if json.Unmarshal(data, &proof) != nil || !device.Encoded(proof["signature"], 64) {
		t.Fatal("missing device proof")
	}
	active := map[string]string{"device_id": deviceID, "state": "active", "revision": "2", "key_thumbprint": "wrong"}
	if _, err := call("confirm", active); err == nil {
		t.Fatal("accepted wrong active key")
	}
	active["key_thumbprint"] = thumb
	if _, err := call("confirm", active); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pending pairing was not consumed")
	}
	hash := sha256.Sum256([]byte(pending.Instance))
	stored, err := os.ReadFile(filepath.Join(dir, hex.EncodeToString(hash[:])+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var confirmed macPairing
	if json.Unmarshal(stored, &confirmed) != nil || confirmed.DeviceID != deviceID || confirmed.ExpiresAt != 0 {
		t.Fatal("invalid confirmed metadata")
	}
}
