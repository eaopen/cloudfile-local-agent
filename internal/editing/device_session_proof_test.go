package editing

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestReadAndClaimProofCannotBeUsedForCommitOrCheckout(t *testing.T) {
	now := time.Now()
	c := DeviceCommitChallenge{
		Instance:  "https://cloudfile.example",
		SessionID: "11111111-1111-4111-8111-111111111111",
		DeviceID:  "22222222-2222-4222-8222-222222222222",
		Operation: "claim",
		Nonce:     base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		IssuedAt:  now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		RequestSHA256: strings.Repeat("a", 64),
	}
	for _, operation := range []string{"claim", "read"} {
		c.Operation = operation
		if err := c.ValidateSessionProof(c.Instance, c.DeviceID, c.SessionID, operation, now); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Message(); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(c.Instance, c.DeviceID, c.SessionID, now); err == nil {
			t.Fatal("read or claim proof accidentally became a Commit")
		}
		if err := c.ValidateLease(c.Instance, c.DeviceID, c.SessionID, now); err == nil {
			t.Fatal("read or claim proof accidentally became a Checkout lease")
		}
	}
	if err := c.ValidateSessionProof(c.Instance, c.DeviceID, c.SessionID, "commit", now); err == nil {
		t.Fatal("Session proof validator accepted write operation")
	}
}
