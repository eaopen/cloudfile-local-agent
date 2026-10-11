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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceLeaseHandoffRequiresDistinctProofAndDurableLocalJournal(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	session := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	lease := Lease{GuardID: "33333333-3333-4333-8333-333333333333",
		Generation: "2", CredentialEpoch: "1", BaseFileID: strings.Repeat("a", 40), Token: strings.Repeat("b", 64)}
	var origin string
	actions := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actions = append(actions, r.URL.Path)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("browser auth forwarded")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if strings.HasSuffix(r.URL.Path, "-challenge/") {
			now := time.Now().Unix()
			c := DeviceCommitChallenge{Instance: origin, DeviceID: device, SessionID: session, Operation: "lease",
				Nonce: base64.RawURLEncoding.EncodeToString(bytesOfLength(32)), IssuedAt: now,
				ExpiresAt: now + 60, RequestSHA256: strings.Repeat("c", 64)}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": c})
			return
		}
		var c DeviceCommitChallenge
		var sig string
		_ = json.Unmarshal(body["challenge"], &c)
		_ = json.Unmarshal(body["signature"], &sig)
		if c.Operation != "lease" || !VerifyP256Signature(&key.PublicKey, c, sig) {
			t.Error("invalid lease proof")
		}
		if strings.HasSuffix(r.URL.Path, "lease-receive/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": session, "lease": lease})
		} else if strings.HasSuffix(r.URL.Path, "lease-confirm/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": session, "confirmed": true})
		} else if strings.HasSuffix(r.URL.Path, "lease-renew/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": session, "renewed": true})
		} else {
			t.Errorf("unexpected route %s", r.URL.Path)
			http.Error(w, "no", 404)
		}
	}))
	defer server.Close()
	origin = server.URL
	broker, err := NewDeviceCommitBroker(server.Client(), origin, session, device,
		func(_ context.Context, c DeviceCommitChallenge) (string, error) {
			return SignLeaseWithP256(key, c, origin, device, session, time.Now())
		})
	if err != nil {
		t.Fatal(err)
	}
	handoff, _ := NewDeviceLeaseBroker(broker)
	got, err := handoff.Receive(context.Background())
	if err != nil || got != lease {
		t.Fatalf("signed lease handoff failed: %+v %v", got, err)
	}
	journal := FileJournal{Path: filepath.Join(t.TempDir(), "editing", session, "journal.json")}
	existing, err := InitializePrivateCheckout(journal, got)
	if err != nil || existing {
		t.Fatalf("journal initialization failed: %v", err)
	}
	if _, err := os.Stat(journal.Path); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Confirm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 6 {
		t.Fatalf("wrong number of device stages %v", actions)
	}
	existed, err := InitializePrivateCheckout(journal, got)
	if err != nil || !existed {
		t.Fatalf("same Checkout retry must be idempotent: %v", err)
	}
	changed := got
	changed.Token = strings.Repeat("f", 64)
	if _, err := InitializePrivateCheckout(journal, changed); err == nil {
		t.Fatal("overwrote existing credential")
	}
	restored, err := journal.Load()
	if err != nil || restored.Lease != got || restored.Pending != nil || restored.SchemaVersion != 1 {
		t.Fatalf("journal not retained: %v", err)
	}
	c := DeviceCommitChallenge{Instance: origin, DeviceID: device, SessionID: session, Operation: "lease",
		Nonce: base64.RawURLEncoding.EncodeToString(bytesOfLength(32)), IssuedAt: time.Now().Unix() - 1,
		ExpiresAt: time.Now().Unix() + 59, RequestSHA256: strings.Repeat("c", 64)}
	if err := c.Validate(origin, device, session, time.Now()); err == nil {
		t.Fatal("lease could impersonate commit")
	}
}

func TestInitializePrivateCheckoutNeverOverwritesPendingJournal(t *testing.T) {
	lease := Lease{GuardID: "33333333-3333-4333-8333-333333333333", Generation: "1",
		CredentialEpoch: "1", BaseFileID: strings.Repeat("a", 40), Token: strings.Repeat("b", 64)}
	journal := FileJournal{Path: filepath.Join(t.TempDir(), "session", "journal.json")}
	if _, err := InitializePrivateCheckout(journal, lease); err != nil {
		t.Fatal(err)
	}
	tampered := lease
	tampered.Generation = "0"
	if _, err := InitializePrivateCheckout(journal, tampered); err == nil {
		t.Fatal("invalid generation accepted")
	}
	original, err := journal.Load()
	if err != nil {
		t.Fatal(err)
	}
	original.Closed = true
	if err := journal.Save(original); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializePrivateCheckout(journal, lease); err == nil {
		t.Fatal("reopened checked-in journal")
	}
}
