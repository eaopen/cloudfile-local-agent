//go:build darwin && cgo

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/mackey"
)

type macPairing struct {
	Instance   string `json:"instance"`
	Alias      string `json:"key_alias"`
	DeviceID   string `json:"device_id"`
	Thumbprint string `json:"thumbprint"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
}

func pairingDirectory() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "CloudFileAgent2", "pairings")
	return dir, os.MkdirAll(dir, 0700)
}
func savePairing(path string, value macPairing) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pairing-*")
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
func pairingInput(value any) error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	if err != nil || len(data) > 16384 {
		return errors.New("bounded pairing response required")
	}
	if strictComponentJSON(data, value) != nil {
		return errors.New("invalid pairing response")
	}
	return nil
}

// Pairing is a local, explicit CLI action. Browser metadata is not authority:
// subsequent operations still need live server verification of device proof.
func runPairing(action, instance, alias string) error {
	dir, err := pairingDirectory()
	if err != nil {
		return err
	}
	return runPairingIn(dir, action, instance, alias)
}

func runPairingIn(dir, action, instance, alias string) error {
	if action == "start" {
		if device.Origin(instance) != nil || alias != "" {
			return errors.New("explicit HTTPS instance required")
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		alias = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
		key, err := mackey.Create(alias)
		if err != nil {
			return err
		}
		defer key.Close()
		thumb, err := key.Thumbprint()
		if err != nil {
			return err
		}
		pub, err := key.PublicJWK()
		if err != nil {
			return err
		}
		pending := macPairing{Instance: instance, Alias: alias, Thumbprint: thumb, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
		if err := savePairing(filepath.Join(dir, "pending-"+alias+".json"), pending); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"key_alias": alias, "instance": instance, "enrollment": map[string]any{"public_key": pub}})
	}
	if !device.ID.MatchString(alias) || instance != "" || (action != "sign" && action != "confirm" && action != "cancel") {
		return errors.New("pairing action and local key alias required")
	}
	path := filepath.Join(dir, "pending-"+alias+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("pending pairing unavailable")
	}
	var pending macPairing
	if strictComponentJSON(data, &pending) != nil || pending.Alias != alias || device.Origin(pending.Instance) != nil {
		return errors.New("pending pairing invalid or expired")
	}
	if action == "cancel" {
		if err := mackey.Delete(alias); err != nil {
			return err
		}
		return os.Remove(path)
	}
	if time.Now().Unix() >= pending.ExpiresAt {
		return errors.New("pending pairing expired")
	}
	key, err := mackey.Open(alias)
	if err != nil {
		return err
	}
	defer key.Close()
	thumb, err := key.Thumbprint()
	if err != nil || thumb != pending.Thumbprint {
		return errors.New("pending device key changed")
	}
	if action == "sign" {
		var response struct {
			DeviceID   string           `json:"device_id"`
			State      string           `json:"state"`
			Revision   string           `json:"revision"`
			Thumbprint string           `json:"key_thumbprint"`
			Challenge  device.Challenge `json:"challenge"`
		}
		if err := pairingInput(&response); err != nil {
			return err
		}
		c := response.Challenge
		if response.State != "pending" || response.Revision != "1" || response.Thumbprint != thumb || c.Validate(pending.Instance, response.DeviceID, c.SessionID, "pair", time.Now()) != nil {
			return errors.New("pair challenge does not match enrollment")
		}
		request, _ := json.Marshal(map[string]string{"device_id": response.DeviceID, "pairing_id": c.SessionID, "key_thumbprint": thumb})
		digest := sha256.Sum256(request)
		if c.RequestSHA256 != hex.EncodeToString(digest[:]) {
			return errors.New("pair request digest mismatch")
		}
		signature, err := key.SignDigest(c.Digest())
		if err != nil {
			return err
		}
		pending.DeviceID = response.DeviceID
		if err := savePairing(path, pending); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"device_id": response.DeviceID, "nonce": c.Nonce, "signature": signature})
	}
	var response struct {
		DeviceID   string `json:"device_id"`
		State      string `json:"state"`
		Revision   string `json:"revision"`
		Thumbprint string `json:"key_thumbprint"`
	}
	if err := pairingInput(&response); err != nil {
		return err
	}
	if !device.ID.MatchString(pending.DeviceID) || response.DeviceID != pending.DeviceID || response.State != "active" || response.Revision != "2" || response.Thumbprint != thumb {
		return errors.New("active pairing confirmation mismatch")
	}
	pending.ExpiresAt = 0
	name := sha256.Sum256([]byte(pending.Instance))
	destination := filepath.Join(dir, hex.EncodeToString(name[:])+".json")
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("instance already paired; explicit removal required before replacement")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := savePairing(destination, pending); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"paired": true, "instance": pending.Instance, "device_id": pending.DeviceID})
}
