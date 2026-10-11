package device

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Origin(value string) error {
	u, err := url.Parse(value)
	if err != nil || len(value) > 255 || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ToLower(u.Host) != u.Host || strings.ContainsAny(value, "%\\?#@") {
		return errors.New("canonical HTTPS origin required")
	}
	if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return errors.New("valid HTTPS host required")
	}
	if raw := u.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("valid HTTPS port required")
		}
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return errors.New("ASCII origin required")
		}
	}
	return nil
}
func Encoded(value string, size int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == size && base64.RawURLEncoding.EncodeToString(raw) == value
}

type Challenge struct {
	Instance      string `json:"instance"`
	DeviceID      string `json:"device_id"`
	SessionID     string `json:"session_id"`
	Operation     string `json:"operation"`
	Nonce         string `json:"nonce"`
	IssuedAt      int64  `json:"issued_at"`
	ExpiresAt     int64  `json:"expires_at"`
	RequestSHA256 string `json:"request_sha256"`
}

func (c Challenge) Validate(instance, deviceID, sessionID, operation string, now time.Time) error {
	allowed := map[string]bool{"pair": true, "claim": true, "read": true, "renew": true, "commit": true, "cancel": true}
	if Origin(c.Instance) != nil || c.Instance != instance || c.DeviceID != deviceID || c.SessionID != sessionID || c.Operation != operation || !allowed[operation] || !ID.MatchString(deviceID) || !ID.MatchString(sessionID) || !Encoded(c.Nonce, 32) || !hash.MatchString(c.RequestSHA256) || c.IssuedAt < 0 || c.ExpiresAt > 9007199254740991 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > 120 || now.Unix() < c.IssuedAt || now.Unix() >= c.ExpiresAt {
		return errors.New("current operation-bound challenge required")
	}
	return nil
}
func (c Challenge) Digest() [32]byte {
	data, _ := json.Marshal(map[string]any{"v": 1, "instance": c.Instance, "device_id": c.DeviceID, "session_id": c.SessionID, "operation": c.Operation, "nonce": c.Nonce, "issued_at": c.IssuedAt, "expires_at": c.ExpiresAt, "request_sha256": c.RequestSHA256})
	return sha256.Sum256(append([]byte("cloudfile.device-proof.v1\n"), data...))
}
