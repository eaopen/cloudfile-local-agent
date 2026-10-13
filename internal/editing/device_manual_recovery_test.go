package editing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lostDevicePublisher struct {
	published Receipt
	calls     int
	queries   int
}

func (p *lostDevicePublisher) PublishDeviceCommit(_ context.Context, _ Lease, i Intent) (Receipt, error) {
	p.calls++
	p.published = Receipt{IntentID: i.ID, State: "published",
		FileID: strings.Repeat("c", 40), ExpectedFileID: i.ExpectedFileID,
		ContentDigest: i.Digest, Action: i.Action, CheckedIn: i.Action == "checkin"}
	return Receipt{}, errors.New("lost native response")
}
func (p *lostDevicePublisher) QueryDeviceCommit(_ context.Context, i Intent) (Receipt, error) {
	p.queries++
	return p.published, nil
}

// Executes the device-authenticated PREPARE and manual CommitCaptured workflow
// with a real HTTPS mock and a simulated lost native publisher acknowledgement.
// Asserts the EXACT previous intent is recovered without a second upload.
func TestDeviceSignedManualWorkflowRecoversPublishedIntent(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	device := "11111111-1111-4111-8111-111111111111"
	session := "22222222-2222-4222-8222-222222222222"
	nonce := base64.RawURLEncoding.EncodeToString(bytesOfLength(32))
	var serverOrigin string
	calls := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("device request leaked browser auth")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var intentID string
		_ = json.Unmarshal(body["intent_id"], &intentID)
		switch {
		case strings.HasSuffix(r.URL.Path, "/commit-challenge/"):
			now := time.Now().Unix()
			challenge := DeviceCommitChallenge{Instance: serverOrigin, DeviceID: device,
				SessionID: session, Operation: "commit", Nonce: nonce, IssuedAt: now, ExpiresAt: now + 60,
				RequestSHA256: strings.Repeat("a", 64)}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": challenge})
		case strings.HasSuffix(r.URL.Path, "/commit-authorize/"):
			var challenge DeviceCommitChallenge
			var signature string
			_ = json.Unmarshal(body["challenge"], &challenge)
			_ = json.Unmarshal(body["signature"], &signature)
			if !VerifyP256Signature(&key.PublicKey, challenge, signature) {
				t.Error("device proof mismatch")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"intent_id": intentID,
				"state": "prepared", "device_authorized": true, "publication": "not_started"})
		default:
			t.Errorf("ordinary, unguarded upload or unexpected API %s", r.URL.Path)
			http.Error(w, "rejected", 400)
		}
	}))
	defer server.Close()
	serverOrigin = server.URL
	broker, err := NewDeviceCommitBroker(server.Client(), serverOrigin, session, device,
		func(_ context.Context, c DeviceCommitChallenge) (string, error) {
			return SignWithP256(key, c, serverOrigin, device, session, time.Now())
		})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &lostDevicePublisher{}
	transport, err := NewDeviceBoundTransport(broker, publisher)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "test.dwg")
	if err := os.WriteFile(work, []byte("explicitly submitted version"), 0600); err != nil {
		t.Fatal(err)
	}
	state := State{Lease: Lease{GuardID: "33333333-3333-4333-8333-333333333333",
		Generation: "2", CredentialEpoch: "1", BaseFileID: strings.Repeat("b", 40), Token: strings.Repeat("a", 64)}}
	journal := FileJournal{Path: filepath.Join(dir, "commit-journal.json")}
	workflow, err := New(state, transport, journal)
	if err != nil {
		t.Fatal(err)
	}
	submitter, _ := NewManualSubmitter(workflow)
	req := SubmitRequest{Trigger: SubmitManual, UserConfirmed: true,
		WorkingCopy: work, PrivateSessionDir: dir, MaxBytes: 1024}
	result, err := submitter.Submit(context.Background(), req)
	if err == nil || result.SnapshotPath == "" {
		t.Fatalf("lost commit must preserve snapshot %+v err=%v", result, err)
	}
	restored, err := journal.Load()
	if err != nil || restored.Pending == nil {
		t.Fatalf("lost intent was not durable %v", err)
	}
	if restored.Pending.ID != publisher.published.IntentID {
		t.Fatal("unknown publication intent differs")
	}
	if err := os.WriteFile(work, []byte("unsubmitted work"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitter.Submit(context.Background(), req); !errors.Is(err, ErrPending) {
		t.Fatalf("created another intent before reconciling %v", err)
	}
	restarted, err := New(restored, transport, journal)
	if err != nil {
		t.Fatal(err)
	}
	recovery, _ := NewManualSubmitter(restarted)
	if err := recovery.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.State().Pending != nil || restarted.State().Closed ||
		restarted.State().Lease.BaseFileID != strings.Repeat("c", 40) {
		t.Fatalf("published intent recovery not applied %+v", restarted.State())
	}
	if publisher.calls != 1 || publisher.queries != 1 || len(calls) != 2 {
		t.Fatalf("recovery retried upload / challenge: native=%d query=%d http=%d", publisher.calls, publisher.queries, len(calls))
	}
	content, err := os.ReadFile(work)
	if err != nil || string(content) != "unsubmitted work" {
		t.Fatal("recovery replaced local unsaved changes")
	}
	snapshot, err := os.ReadFile(result.SnapshotPath)
	if err != nil || string(snapshot) != "explicitly submitted version" {
		t.Fatal("lost original snapshot")
	}
}
