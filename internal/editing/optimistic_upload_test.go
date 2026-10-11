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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// True same-origin TLS device signer + exact-one-file bytes over the normal
// optimistic session route, with no Checkout token or browser cookie.
func TestOptimisticUploadRequiresSignedDeviceAndOriginalVersion(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	const ticket = "v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var origin string
	copyBytes := []byte("manually saved Office/CAD optimistic edit")
	filename := filepath.Join(t.TempDir(), "working-copy-with-local-name.dwg")
	if err := os.WriteFile(filename, copyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := CaptureSnapshot(filename, t.TempDir(), 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{ID: sid, Action: "optimistic-update", Snapshot: snap.Path,
		SnapshotSize: snap.Size, Digest: snap.Digest, ExpectedFileID: strings.Repeat("a", 40)}
	proofs, transfers := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("device upload leaked browser Cookie")
		}
		switch r.URL.Path {
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload-challenge/":
			proofs++
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil ||
				body["base_file_id"] != intent.ExpectedFileID ||
				body["content_digest"] != intent.Digest ||
				body["snapshot_size"] != float64(len(copyBytes)) {
				t.Error("initial signed data did not bind the original file")
			}
			c := DeviceCommitChallenge{Instance: origin, SessionID: sid, DeviceID: did,
				Operation: "commit", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
				IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(),
				RequestSHA256: strings.Repeat("f", 64)}
			json.NewEncoder(w).Encode(map[string]any{"challenge": c})
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload-authorize/":
			proofs++
			var body struct {
				Challenge DeviceCommitChallenge `json:"challenge"`
				Signature string                `json:"signature"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil ||
				!VerifyP256Signature(&key.PublicKey, body.Challenge, body.Signature) {
				t.Error("paired P256 signature missing")
			}
			json.NewEncoder(w).Encode(map[string]any{"ticket": ticket,
				"expires_in": 60, "intent_id": intent.ID, "content_digest": intent.Digest,
				"snapshot_size": intent.SnapshotSize, "native_device_publish": true,
				"transfer": map[string]any{
					"path":   "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/",
					"method": "POST", "authorization": "Bearer", "redirects": false}})
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/":
			transfers++
			if r.Header.Get("Authorization") != "Bearer "+ticket || r.Header.Get("Idempotency-Key") != intent.ID {
				t.Error("upload did not use approved intent-scoped device token")
			}
			got, _ := io.ReadAll(r.Body)
			if string(got) != string(copyBytes) || r.ContentLength != int64(len(copyBytes)) {
				t.Error("snapshot bytes did not match exactly")
			}
			result := OptimisticResult{IntentID: intent.ID, State: "published",
				FileID: strings.Repeat("b", 40), BaseFileID: intent.ExpectedFileID}
			json.NewEncoder(w).Encode(result)
		default:
			t.Errorf("unexpected optimistic route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	origin = server.URL
	broker, err := NewDeviceCommitBroker(server.Client(), origin, sid, did,
		func(_ context.Context, c DeviceCommitChallenge) (string, error) {
			return SignWithP256(key, c, origin, did, sid, time.Now())
		})
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := NewOptimisticUploader(broker)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := uploader.Upload(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if proofs != 2 || transfers != 1 || receipt.State != "published" ||
		receipt.BaseFileID != intent.ExpectedFileID {
		t.Fatalf("unknown optimistic completion %+v proofs=%d transfers=%d", receipt, proofs, transfers)
	}
	bytesOnDisk, _ := os.ReadFile(snap.Path)
	sum := sha256.Sum256(bytesOnDisk)
	if hex.EncodeToString(sum[:]) != intent.Digest {
		t.Fatal("protected local snapshot mutated")
	}
}

func TestOptimisticUploadRejectsUnboundIntentBeforeNetwork(t *testing.T) {
	intent := Intent{ID: "11111111-1111-4111-8111-111111111111", Action: "commit",
		ExpectedFileID: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64)}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("must not send bytes without optimistic intent")
	}))
	defer server.Close()
	b, _ := NewDeviceCommitBroker(server.Client(), server.URL, intent.ID,
		"22222222-2222-4222-8222-222222222222",
		func(_ context.Context, c DeviceCommitChallenge) (string, error) {
			return SignWithP256(key, c, server.URL, c.DeviceID, c.SessionID, time.Now())
		})
	uploader, _ := NewOptimisticUploader(b)
	if _, err := uploader.Upload(context.Background(), intent); err == nil {
		t.Fatal("exclusive action was confused with optimistic")
	}
}
