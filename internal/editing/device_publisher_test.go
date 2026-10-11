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

func TestDeviceUploadLostReplyRecoversOriginalIntent(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	const ticket = "v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var origin string
	var prepared map[string]json.RawMessage
	var receipt Receipt
	uploads := 0
	working := filepath.Join(t.TempDir(), "user-secret-filename.docx")
	os.WriteFile(working, []byte("captured manual changes"), 0600)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("browser Cookie forwarded")
		}
		if r.URL.Path == "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/" {
			uploads++
			if r.Header.Get("Authorization") != "Bearer "+ticket || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Error("wrong dedicated byte credential")
			}
			data, _ := io.ReadAll(r.Body)
			if string(data) != "captured manual changes" {
				t.Error("working copy mutation changed captured upload")
			}
			if r.ContentLength != int64(len(data)) {
				t.Error("unbounded or wrong upload length")
			}
			var intent, base, digest, action string
			json.Unmarshal(prepared["intent_id"], &intent)
			json.Unmarshal(prepared["base_file_id"], &base)
			json.Unmarshal(prepared["content_digest"], &digest)
			json.Unmarshal(prepared["action"], &action)
			if r.Header.Get("Idempotency-Key") != intent {
				t.Error("upload intent changed")
			}
			receipt = Receipt{IntentID: intent, State: "published", FileID: strings.Repeat("f", 40), ContentDigest: digest, ExpectedFileID: base, Action: action, CheckedIn: action == "checkin"}
			// Native publication happened; simulate a lost/unconfirmed response.
			http.Error(w, "lost receipt", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer sent to proof endpoint")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "user-secret-filename") || strings.Contains(string(raw), working) {
			t.Error("local path leaked")
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil {
			t.Error("invalid JSON")
			http.Error(w, "bad", 400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "challenge/") {
			digest := sha256.Sum256(raw)
			c := DeviceCommitChallenge{Instance: origin, DeviceID: did, SessionID: sid, Operation: "commit", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), RequestSHA256: hex.EncodeToString(digest[:])}
			json.NewEncoder(w).Encode(map[string]any{"challenge": c})
			return
		}
		var c DeviceCommitChallenge
		var signature string
		json.Unmarshal(body["challenge"], &c)
		json.Unmarshal(body["signature"], &signature)
		if !VerifyP256Signature(&key.PublicKey, c, signature) {
			t.Error("signature missing or invalid")
		}
		switch r.URL.Path {
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-authorize/":
			prepared = body
			var id string
			json.Unmarshal(body["intent_id"], &id)
			json.NewEncoder(w).Encode(map[string]any{"intent_id": id, "state": "prepared", "device_authorized": true, "publication": "not_started"})
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload-authorize/":
			var id, digest string
			var size int64
			json.Unmarshal(body["intent_id"], &id)
			json.Unmarshal(body["content_digest"], &digest)
			json.Unmarshal(body["snapshot_size"], &size)
			// A save while authorization is in flight must not alter approved bytes.
			os.WriteFile(working, []byte("later unsaved changes"), 0600)
			json.NewEncoder(w).Encode(map[string]any{"ticket": ticket, "expires_in": 60, "intent_id": id, "content_digest": digest, "snapshot_size": size, "native_device_publish": true, "transfer": map[string]any{"path": "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/", "method": "POST", "authorization": "Bearer", "redirects": false}})
		case "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-status/":
			var id string
			json.Unmarshal(body["intent_id"], &id)
			if id != receipt.IntentID {
				t.Error("recovery queried a new intent")
			}
			json.NewEncoder(w).Encode(map[string]any{"intent": receipt})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.Error(w, "no", 404)
		}
	}))
	defer server.Close()
	origin = server.URL
	broker, err := NewDeviceCommitBroker(server.Client(), origin, sid, did, func(_ context.Context, c DeviceCommitChallenge) (string, error) {
		return SignWithP256(key, c, origin, did, sid, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewDeviceHTTPPublisher(broker)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewDeviceBoundTransport(broker, publisher)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	journal := FileJournal{Path: filepath.Join(dir, "journal.json")}
	state := State{Lease: Lease{GuardID: "44444444-4444-4444-8444-444444444444", Generation: "1", CredentialEpoch: "1", Token: strings.Repeat("a", 64), BaseFileID: strings.Repeat("b", 40)}}
	if err := journal.Save(state); err != nil {
		t.Fatal(err)
	}
	worker, err := OpenSessionWorker(journal, transport)
	if err != nil {
		t.Fatal(err)
	}
	submitter, _ := NewManualSubmitter(worker.Workflow)
	result, err := submitter.Submit(context.Background(), SubmitRequest{Trigger: SubmitManual, UserConfirmed: true, WorkingCopy: working, PrivateSessionDir: dir, MaxBytes: 1024, FinishEditing: true})
	if err == nil || worker.Workflow.State().Pending == nil {
		t.Fatal("unknown reply reported success")
	}
	if _, err := os.Stat(result.SnapshotPath); err != nil {
		t.Fatal("lost-response snapshot discarded")
	}
	worker.Close()
	// A new process/workflow has no in-memory receipt to rely on.
	worker, err = OpenSessionWorker(journal, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	submitter, _ = NewManualSubmitter(worker.Workflow)
	if err := submitter.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := worker.Workflow.State()
	if !after.Closed || after.Pending != nil || after.LastReceipt == nil || uploads != 1 {
		t.Fatalf("recovery repeated upload or lost receipt: %#v uploads=%d", after, uploads)
	}
	data, _ := os.ReadFile(working)
	if string(data) != "later unsaved changes" {
		t.Fatal("recovery overwrote working file")
	}
}

func TestDeviceReceiptCannotPromotePreparedToSaved(t *testing.T) {
	intent := Intent{ID: "11111111-1111-4111-8111-111111111111", Action: "commit", Digest: strings.Repeat("a", 64), ExpectedFileID: strings.Repeat("b", 40)}
	for _, r := range []Receipt{
		{IntentID: intent.ID, State: "prepared"},
		{IntentID: intent.ID, State: "published", FileID: strings.Repeat("f", 40), ContentDigest: intent.Digest, ExpectedFileID: intent.ExpectedFileID, Action: "commit", CheckedIn: true},
		{IntentID: intent.ID, State: "published", FileID: strings.Repeat("f", 40), ContentDigest: strings.Repeat("c", 64), ExpectedFileID: intent.ExpectedFileID, Action: "commit"},
	} {
		if validateDeviceReceipt(r, intent, true) == nil {
			t.Fatal("unbound success receipt accepted")
		}
	}
}

func TestDeviceUploadRejectsUnboundGrantsBeforeSendingBytes(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	for _, failure := range []string{"disabled", "wrong_intent", "wrong_digest", "wrong_size", "redirect", "wrong_path", "expired", "missing_route"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot")
			os.WriteFile(path, []byte("bytes"), 0400)
			digest := sha256.Sum256([]byte("bytes"))
			intent := Intent{ID: sid, Snapshot: path, SnapshotSize: 5, Digest: hex.EncodeToString(digest[:]), ExpectedFileID: strings.Repeat("b", 40), Action: "commit"}
			lease := Lease{GuardID: sid, Generation: "1", CredentialEpoch: "1", Token: strings.Repeat("a", 64), BaseFileID: intent.ExpectedFileID}
			var origin string
			uploads := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "commit-challenge/") {
					json.NewEncoder(w).Encode(map[string]any{"challenge": DeviceCommitChallenge{Instance: origin, DeviceID: did, SessionID: sid, Operation: "commit", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), RequestSHA256: strings.Repeat("a", 64)}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "commit-upload-authorize/") {
					if failure == "missing_route" {
						http.NotFound(w, r)
						return
					}
					transfer := map[string]any{"path": "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/", "method": "POST", "authorization": "Bearer", "redirects": false}
					grant := map[string]any{"ticket": "v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expires_in": 60, "intent_id": intent.ID, "content_digest": intent.Digest, "snapshot_size": 5, "native_device_publish": true, "transfer": transfer}
					switch failure {
					case "disabled":
						grant["native_device_publish"] = false
					case "wrong_intent":
						grant["intent_id"] = did
					case "wrong_digest":
						grant["content_digest"] = strings.Repeat("c", 64)
					case "wrong_size":
						grant["snapshot_size"] = 6
					case "redirect":
						transfer["redirects"] = true
					case "wrong_path":
						transfer["path"] = "/api2/upload"
					case "expired":
						grant["expires_in"] = 0
					}
					json.NewEncoder(w).Encode(grant)
					return
				}
				uploads++
				http.Error(w, "unexpected bytes", 500)
			}))
			defer server.Close()
			origin = server.URL
			broker, err := NewDeviceCommitBroker(server.Client(), origin, sid, did, func(context.Context, DeviceCommitChallenge) (string, error) {
				return base64.RawURLEncoding.EncodeToString(make([]byte, 64)), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			publisher, _ := NewDeviceHTTPPublisher(broker)
			if _, err := publisher.PublishDeviceCommit(context.Background(), lease, intent); err == nil {
				t.Fatal("invalid native grant accepted")
			}
			if uploads != 0 {
				t.Fatal("bytes sent without bound native grant")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 || entries[0].Name() != "snapshot" {
				t.Fatal("temporary upload copy not cleaned")
			}
		})
	}
}
