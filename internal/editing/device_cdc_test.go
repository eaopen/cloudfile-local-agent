package editing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
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

	"github.com/eaopen/cloudfile-local-agent/internal/blockdelta"
)

func TestOptInDeviceCDCUploadsOnlyMissingBlocksBeforeGuardedPublish(t *testing.T) {
	t.Setenv("CLOUDFILE_AGENT2_CDC_ENABLED", "1")
	const sid = "11111111-1111-4111-8111-111111111111"
	const did = "22222222-2222-4222-8222-222222222222"
	const ticket = "v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := filepath.Join(t.TempDir(), "pinned.snapshot")
	parts := [][]byte{make([]byte, 10<<20), make([]byte, 10<<20), make([]byte, 3<<20)}
	for i := range parts {
		for k := range parts[i] {
			parts[i][k] = byte(i + 31)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		if _, err = f.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := blockdelta.ScanSnapshot(path, 1)
	if err != nil || len(manifest.Blocks) < 2 {
		t.Fatalf("test CDC manifest: %+v %v", manifest, err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lease := Lease{GuardID: sid, Generation: "1", CredentialEpoch: "1", Token: strings.Repeat("a", 64), BaseFileID: strings.Repeat("b", 40)}
	intent := Intent{ID: sid, Snapshot: path, SnapshotSize: manifest.Size, Digest: manifest.SHA256, ExpectedFileID: lease.BaseFileID, Action: "checkin"}
	missing := manifest.Blocks[len(manifest.Blocks)-1]
	var origin string
	var stageBytes int64
	stages, planCalls, publishCalls, untrustedCalls := 0, 0, 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("browser cookie forwarded")
		}
		route := r.URL.Path
		if strings.HasSuffix(route, "commit-challenge/") {
			c := DeviceCommitChallenge{Instance: origin, DeviceID: did, SessionID: sid, Operation: "commit",
				Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: time.Now().Unix(),
				ExpiresAt: time.Now().Add(time.Minute).Unix(), RequestSHA256: strings.Repeat("c", 64)}
			json.NewEncoder(w).Encode(map[string]any{"challenge": c})
			return
		}
		if strings.HasSuffix(route, "commit-upload-authorize/") {
			var req struct {
				Challenge DeviceCommitChallenge `json:"challenge"`
				Signature string                `json:"signature"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || !VerifyP256Signature(&key.PublicKey, req.Challenge, req.Signature) {
				t.Error("invalid device challenge signature")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"ticket": ticket, "expires_in": 60, "intent_id": intent.ID, "content_digest": intent.Digest,
				"snapshot_size": intent.SnapshotSize, "native_device_publish": true,
				"transfer": map[string]any{"path": "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/",
					"method": "POST", "authorization": "Bearer", "redirects": false}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+ticket || r.Header.Get("Idempotency-Key") != intent.ID {
			t.Error("bad scoped Bearer or Intent")
		}
		switch {
		case strings.HasSuffix(route, "commit-block-plan/"):
			planCalls++
			var plan struct {
				FileID string `json:"file_id"`
				Size   int64  `json:"size"`
			}
			if json.NewDecoder(r.Body).Decode(&plan) != nil || plan.FileID != manifest.FileID || plan.Size != manifest.Size {
				t.Error("wrong immutable CDC plan")
			}
			json.NewEncoder(w).Encode(map[string]any{"file_id": manifest.FileID,
				"snapshot_size": manifest.Size, "intent_id": intent.ID, "missing": []string{missing.ID}})
		case strings.HasSuffix(route, "commit-block-stage/"):
			stages++
			data, _ := io.ReadAll(r.Body)
			digest := sha1.Sum(data)
			if r.Header.Get("X-CloudFile-Block-ID") != missing.ID ||
				hex.EncodeToString(digest[:]) != missing.ID || len(data) != int(missing.Size) {
				t.Error("staged unexpected raw block bytes")
			}
			stageBytes += int64(len(data))
			json.NewEncoder(w).Encode(map[string]any{"staged": true, "block_id": missing.ID, "intent_id": intent.ID})
		case strings.HasSuffix(route, "commit-block-publish/"):
			publishCalls++
			json.NewEncoder(w).Encode(map[string]any{"intent": Receipt{IntentID: intent.ID,
				State: "published", FileID: manifest.FileID, ContentDigest: intent.Digest,
				ExpectedFileID: intent.ExpectedFileID, Action: intent.Action, CheckedIn: true}})
		default:
			untrustedCalls++
			http.Error(w, "not an authorized CDC route", 404)
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
	publisher, _ := NewDeviceHTTPPublisher(broker)
	receipt, err := publisher.PublishDeviceCommit(context.Background(), lease, intent)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != "published" || receipt.FileID != manifest.FileID || !receipt.CheckedIn ||
		planCalls != 1 || stages != 1 || publishCalls != 1 || untrustedCalls != 0 ||
		stageBytes != missing.Size || stageBytes >= intent.SnapshotSize {
		t.Fatalf("CDC incorrect publication %+v bytes=%d stages=%d plan=%d publish=%d untrusted=%d",
			receipt, stageBytes, stages, planCalls, publishCalls, untrustedCalls)
	}
	// The captured immutable content digest matches the same whole-file bytes.
	raw, _ := os.ReadFile(path)
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != receipt.ContentDigest {
		t.Fatal("snapshot digest changed")
	}
}

func TestCDCTransportChoiceAvoidsColdOrCompressedFullRewrite(t *testing.T) {
	cases := []struct {
		total, missing int64
		want           bool
	}{
		{20 << 20, 5 << 20, true},
		{20 << 20, 0, true},
		{20 << 20, 20 << 20, false},
		{20 << 20, 16 << 20, false},
		{11 << 20, 1 << 20, false},
		{40 << 20, 27 << 20, true},
		{40 << 20, -1, false},
	}
	for _, c := range cases {
		if got := cdcWorthwhile(c.total, c.missing); got != c.want {
			t.Fatalf("CDC choice bytes=%d missing=%d got=%t want=%t",
				c.total, c.missing, got, c.want)
		}
	}
}
