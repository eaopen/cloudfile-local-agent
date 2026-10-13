package editing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBrokerChallengesExactCheckoutAndPreparesIntentWithoutUpload(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	session := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	intentID := "33333333-3333-4333-8333-333333333333"
	guardID := "44444444-4444-4444-8444-444444444444"
	nonce := base64.RawURLEncoding.EncodeToString(bytesOfLength(32))
	now := time.Now().Unix()
	var origin string
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("browser credentials were forwarded")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/commit-challenge/") {
			if _, ok := body["signature"]; ok {
				t.Error("signed request sent to challenge path")
			}
			var target string
			_ = json.Unmarshal(body["intent_id"], &target)
			if target != intentID {
				t.Error("intent changed")
			}
			challenge := DeviceCommitChallenge{Instance: origin, DeviceID: device, SessionID: session, Operation: "commit",
				Nonce: nonce, IssuedAt: now, ExpiresAt: now + 60, RequestSHA256: strings.Repeat("a", 64)}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": challenge})
		} else if strings.HasSuffix(r.URL.Path, "/commit-authorize/") {
			var challenge DeviceCommitChallenge
			_ = json.Unmarshal(body["challenge"], &challenge)
			var signature string
			_ = json.Unmarshal(body["signature"], &signature)
			if !VerifyP256Signature(&key.PublicKey, challenge, signature) {
				t.Error("challenge signature invalid")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"intent_id": intentID, "state": "prepared", "device_authorized": true, "publication": "not_started"})
		} else {
			t.Errorf("unpermitted broker endpoint: %s", r.URL.Path)
			http.Error(w, "no", 404)
		}
	}))
	defer server.Close()
	origin = server.URL
	broker, err := NewDeviceCommitBroker(server.Client(), origin, session, device, func(_ context.Context, c DeviceCommitChallenge) (string, error) {
		return SignWithP256(key, c, origin, device, session, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	content := sha256.Sum256([]byte("manual file"))
	snapshot := Intent{ID: intentID, Digest: hex.EncodeToString(content[:]), Action: "commit",
		ExpectedFileID: strings.Repeat("b", 40), SnapshotSize: 11}
	lease := Lease{GuardID: guardID, Generation: "1", CredentialEpoch: "1",
		BaseFileID: strings.Repeat("b", 40), Token: strings.Repeat("a", 64)}
	if err = broker.PrepareIntent(context.Background(), lease, snapshot); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("unexpected network operations %d", requests)
	}
}

func TestDeviceBrokerRejectsUnsafeOriginAndInvalidIntent(t *testing.T) {
	client := &http.Client{}
	signer := func(context.Context, DeviceCommitChallenge) (string, error) { return "", nil }
	sid := "11111111-1111-4111-8111-111111111111"
	for _, bad := range []string{"http://domain.test/path", "https://domain.test/path", "https://user@domain.test", "ftp://domain.test", "http://other@domain.test"} {
		if _, err := NewDeviceCommitBroker(client, bad, sid, sid, signer); err == nil {
			t.Errorf("accepted origin %s", bad)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("invalid intent made HTTP request") }))
	defer server.Close()
	_, err := NewDeviceCommitBroker(server.Client(), server.URL, sid, sid, signer)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(server.URL)
	if parsed.Scheme != "https" {
		t.Fatal("TLS fixture not used")
	}
	broker, _ := NewDeviceCommitBroker(server.Client(), server.URL, sid, sid, signer)
	if err = broker.PrepareIntent(context.Background(), Lease{}, Intent{}); err == nil {
		t.Fatal("accepted missing checkout")
	}
}
