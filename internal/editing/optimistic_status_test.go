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

func TestOptimisticRecoveryQueriesSameSignedIntentWithoutBytes(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	original := Intent{ID: "33333333-3333-4333-8333-333333333333",
		Action: "optimistic-update", ExpectedFileID: strings.Repeat("a", 40),
		Digest: strings.Repeat("b", 64), SnapshotSize: 123}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var origin string
	reads, uploadBytes := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("status read must be device proof only")
		}
		if r.URL.Path == "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-status-challenge/" {
			c := DeviceCommitChallenge{Instance: origin, DeviceID: did, SessionID: sid,
				Operation: "commit", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
				IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(),
				RequestSHA256: strings.Repeat("e", 64)}
			json.NewEncoder(w).Encode(map[string]any{"challenge": c})
			return
		}
		reads++
		if r.URL.Path != "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-status/" {
			uploadBytes++
			t.Errorf("query tried upload route %s", r.URL.Path)
			return
		}
		var body struct {
			IntentID  string                `json:"intent_id"`
			Challenge DeviceCommitChallenge `json:"challenge"`
			Signature string                `json:"signature"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.IntentID != original.ID ||
			!VerifyP256Signature(&key.PublicKey, body.Challenge, body.Signature) {
			t.Error("wrong status intent or signature")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"intent": map[string]any{"intent_id": original.ID, "state": "published",
				"base_file_id":   original.ExpectedFileID,
				"content_digest": original.Digest,
				"snapshot_size":  original.SnapshotSize,
				"result_file_id": strings.Repeat("c", 40)}})
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
	worker, _ := NewOptimisticUploader(broker)
	result, err := worker.Query(context.Background(), original)
	if err != nil || result.State != "published" ||
		result.FileID != strings.Repeat("c", 40) || reads != 1 || uploadBytes != 0 {
		t.Fatalf("signed original intent Query failed %+v %v reads=%d upload=%d",
			result, err, reads, uploadBytes)
	}
}
