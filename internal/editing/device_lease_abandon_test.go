package editing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignedCheckoutAbandonRequiresOriginalGuardAndCurrentReceipt(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lease := Lease{GuardID: "33333333-3333-4333-8333-333333333333",
		Generation: "2", CredentialEpoch: "1", BaseFileID: strings.Repeat("a", 40),
		Token: strings.Repeat("b", 64)}
	var origin string
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("lease-abandon must not send a browser cookie or bearer")
		}
		calls++
		if r.URL.Path == "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/lease-abandon-challenge/" {
			c := DeviceCommitChallenge{Instance: origin, DeviceID: did, SessionID: sid,
				Operation: "lease", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
				IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(),
				RequestSHA256: strings.Repeat("d", 64)}
			json.NewEncoder(w).Encode(map[string]any{"challenge": c})
			return
		}
		if r.URL.Path != "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/lease-abandon/" {
			t.Error("unrecognized lock release path")
			return
		}
		var req struct {
			GuardID   string                `json:"guard_id"`
			Token     string                `json:"token"`
			Challenge DeviceCommitChallenge `json:"challenge"`
			Signature string                `json:"signature"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.GuardID != lease.GuardID ||
			req.Token != lease.Token || !VerifyP256Signature(&key.PublicKey, req.Challenge, req.Signature) {
			t.Error("original Checkout proof not bound to paired signature")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": sid, "state": "abandoned", "released": true})
	}))
	defer server.Close()
	origin = server.URL
	broker, _ := NewDeviceCommitBroker(server.Client(), origin, sid, did,
		func(_ context.Context, c DeviceCommitChallenge) (string, error) {
			return SignLeaseWithP256(key, c, origin, did, sid, time.Now())
		})
	if err := broker.AbandonCheckout(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected one signed challenge and one abandon, got %d", calls)
	}
	wrong := lease
	wrong.Token = "invalid"
	if broker.AbandonCheckout(context.Background(), wrong) == nil {
		t.Fatal("noncanonical Checkout credential accepted")
	}
	if calls != 2 {
		t.Fatal("invalid local Checkout triggered network operation")
	}
}
