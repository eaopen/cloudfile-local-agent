//go:build windows

package editing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// SignCommitWithWindowsBridge delegates *only* server-created commit device
// challenges to the existing .NET/CNG current-user device key. The helper is
// shipped with Windows Agent1 1.2.1, which continues to own the native UI.
// No browser cookie, raw file bytes or Seafile password enters the bridge.
func SignLeaseWithWindowsBridge(ctx context.Context, exe string, challenge DeviceCommitChallenge,
	instance, deviceID, sessionID string) (string, error) {
	return signWithWindowsBridge(ctx, exe, challenge, instance, deviceID, sessionID, "lease")
}

func SignCommitWithWindowsBridge(ctx context.Context, exe string,
	challenge DeviceCommitChallenge, instance, deviceID, sessionID string) (string, error) {
	return signWithWindowsBridge(ctx, exe, challenge, instance, deviceID, sessionID, "commit")
}

func signWithWindowsBridge(ctx context.Context, exe string,
	challenge DeviceCommitChallenge, instance, deviceID, sessionID, operation string) (string, error) {
	var err error
	if operation == "lease" {
		err = challenge.ValidateLease(instance, deviceID, sessionID, time.Now())
	} else if operation == "commit" {
		err = challenge.Validate(instance, deviceID, sessionID, time.Now())
	} else {
		return "", errors.New("unsupported fixed device proof")
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(exe) || filepath.Ext(exe) != ".exe" {
		return "", errors.New("installed absolute Windows bridge required")
	}
	info, err := os.Lstat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("trusted Windows bridge binary required")
	}
	body, err := json.Marshal(map[string]any{"instance": instance, "device_id": deviceID,
		"session_id": sessionID, "challenge": challenge})
	if err != nil || len(body) > 8192 {
		return "", errors.New("device bridge payload exceeded limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, exe, "--sign-"+operation)
	command.Stdin = bytes.NewReader(body)
	command.Stderr = io.Discard
	var output bytes.Buffer
	command.Stdout = &boundedWriter{out: &output, max: 8192}
	if err := command.Run(); err != nil {
		return "", errors.New("device bridge proof was not confirmed")
	}
	var reply struct {
		OK        bool   `json:"ok"`
		Signature string `json:"signature"`
		DeviceID  string `json:"device_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(output.Bytes(), &reply); err != nil || !reply.OK ||
		reply.DeviceID != deviceID || reply.SessionID != sessionID {
		return "", errors.New("device bridge response rejected")
	}
	raw, err := base64.RawURLEncoding.DecodeString(reply.Signature)
	if err != nil || len(raw) != 64 || base64.RawURLEncoding.EncodeToString(raw) != reply.Signature {
		return "", errors.New("invalid P-256 bridge signature")
	}
	return reply.Signature, nil
}

type boundedWriter struct {
	out *bytes.Buffer
	max int
}

func (w *boundedWriter) Write(b []byte) (int, error) {
	if len(b) > w.max-w.out.Len() {
		return 0, errors.New("bridge output exceeded limit")
	}
	return w.out.Write(b)
}
