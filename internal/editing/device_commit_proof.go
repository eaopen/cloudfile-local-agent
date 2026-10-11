package editing

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var proofUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var proofSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DeviceCommitChallenge mirrors CloudFile's saved P-256 one-use challenge.
// Signing these bytes proves possession, not write authority. The server must
// consume the challenge and re-evaluate current actor, ACL, checkout, source,
// resource baseline and CommitIntent in the same guarded publication workflow.
type DeviceCommitChallenge struct {
	Instance      string `json:"instance"`
	DeviceID      string `json:"device_id"`
	SessionID     string `json:"session_id"`
	Operation     string `json:"operation"`
	Nonce         string `json:"nonce"`
	IssuedAt      int64  `json:"issued_at"`
	ExpiresAt     int64  `json:"expires_at"`
	RequestSHA256 string `json:"request_sha256"`
}

func (c DeviceCommitChallenge) Validate(instance, deviceID, sessionID string, now time.Time) error {
	parsed, err := url.Parse(c.Instance)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || strings.HasSuffix(c.Instance, "/") ||
		strings.ToLower(parsed.Host) != parsed.Host || strings.ContainsAny(c.Instance, "%\\") ||
		c.Instance != instance || c.DeviceID != deviceID || c.SessionID != sessionID || c.Operation != "commit" ||
		len(c.Instance) > 255 || !proofUUID.MatchString(c.DeviceID) || !proofUUID.MatchString(c.SessionID) ||
		!proofSHA.MatchString(c.RequestSHA256) || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > 120 ||
		now.Unix() < c.IssuedAt || now.Unix() >= c.ExpiresAt {
		return errors.New("current fixed-origin commit challenge required")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(c.Nonce)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != c.Nonce {
		return errors.New("canonical device challenge nonce required")
	}
	return nil
}

func (c DeviceCommitChallenge) Message() ([]byte, error) {
	// json.Marshal sorts object keys, matching the Python DeviceChallenge.message
	// canonical sorted compact JSON; no private credentials enter the message.
	if c.Operation != "commit" {
		return nil, errors.New("unsupported signing action")
	}
	obj := map[string]any{
		"v": 1, "instance": c.Instance, "device_id": c.DeviceID,
		"session_id": c.SessionID, "operation": c.Operation, "nonce": c.Nonce,
		"issued_at": c.IssuedAt, "expires_at": c.ExpiresAt,
		"request_sha256": c.RequestSHA256,
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return append([]byte("cloudfile.device-proof.v1\n"), body...), nil
}

// SignWithP256 is the native Go/macOS signing adapter when the host supplies a
// protected P-256 signer. Do not serialize private keys or install a user
// unprotected PEM file as part of this application.
func SignWithP256(key *ecdsa.PrivateKey, c DeviceCommitChallenge, instance, deviceID, sessionID string, now time.Time) (string, error) {
	if key == nil || key.Curve == nil || key.Curve.Params().Name != "P-256" {
		return "", errors.New("P-256 device signing key required")
	}
	if err := c.Validate(instance, deviceID, sessionID, now); err != nil {
		return "", err
	}
	message, err := c.Message()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	var raw [64]byte
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func VerifyP256Signature(publicKey *ecdsa.PublicKey, c DeviceCommitChallenge, signature string) bool {
	if publicKey == nil || publicKey.Curve == nil || publicKey.Curve.Params().Name != "P-256" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(raw) != 64 || base64.RawURLEncoding.EncodeToString(raw) != signature {
		return false
	}
	msg, err := c.Message()
	if err != nil {
		return false
	}
	digest := sha256.Sum256(msg)
	return ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:]))
}
