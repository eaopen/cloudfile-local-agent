package device

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestChallengeBindings(t *testing.T) {
	now := time.Now()
	id := "11111111-1111-4111-8111-111111111111"
	c := Challenge{Instance: "https://cloud.example", DeviceID: id, SessionID: id, Operation: "pair", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), RequestSHA256: strings.Repeat("a", 64)}
	if err := c.Validate(c.Instance, id, id, "pair", now); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "commit", "arbitrary"} {
		if c.Validate(c.Instance, id, id, op, now) == nil {
			t.Fatal("cross-operation challenge accepted")
		}
	}
	if c.Validate("https://other.example", id, id, "pair", now) == nil {
		t.Fatal("cross-origin challenge accepted")
	}
	if c.Validate(c.Instance, id, id, "pair", now.Add(time.Minute)) == nil {
		t.Fatal("expired challenge accepted")
	}
	for _, origin := range []string{"http://cloud.example", "https://cloud.example/", "https://user@cloud.example", "https://cloud.example?", "https://Cloud.example", "https://cloud.example:99999", "https://cloud.example:"} {
		if Origin(origin) == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
}
