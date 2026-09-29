package editing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureLease() Lease {
	return Lease{GuardID: "11111111-1111-4111-8111-111111111111", Generation: "1", CredentialEpoch: "1", BaseFileID: strings.Repeat("b", 40), Token: strings.Repeat("a", 64)}
}
func fixtureReference() Reference {
	return Reference{RepoID: "22222222-2222-4222-8222-222222222222", Path: "/sample.dwg", Kind: "file"}
}
func TestHTTPManualSnapshotAndHistoricalCheckin(t *testing.T) {
	lease := fixtureLease()
	var receipt Receipt
	var uploaded []byte
	publishes := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/editing/status/":
			var body struct {
				IntentID string `json:"intent_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.IntentID != "" {
				json.NewEncoder(w).Encode(receipt)
				return
			}
			json.NewEncoder(w).Encode(Guard{Lease: lease, Active: true, Mode: "checkout"})
		case "/editing/commit-file/":
			if r.ContentLength <= 0 {
				t.Error("missing multipart content length")
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if r.FormValue("token") != lease.Token || r.FormValue("generation") != "1" || r.FormValue("source_identity") == "" {
				t.Error("missing bound proof or work copy identity")
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			var buffer strings.Builder
			_, err = io.Copy(&buffer, file)
			if err != nil {
				t.Error(err)
			}
			uploaded = []byte(buffer.String())
			digest := sha256.Sum256(uploaded)
			receipt = Receipt{IntentID: r.FormValue("intent_id"), State: "published", FileID: strings.Repeat("c", 40), ExpectedFileID: lease.BaseFileID, ContentDigest: hex.EncodeToString(digest[:]), Action: r.FormValue("action")}
			publishes++
			lease.BaseFileID = receipt.FileID
			// A real transport failure AFTER this fixture accepted publication.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		case "/editing/checkin/":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			digest := sha256.Sum256(nil)
			receipt = Receipt{IntentID: body["intent_id"], State: "published", FileID: lease.BaseFileID, ExpectedFileID: lease.BaseFileID, ContentDigest: hex.EncodeToString(digest[:]), Action: "checkin-unchanged", CheckedIn: true}
			json.NewEncoder(w).Encode(map[string]any{"intent": receipt})
		default:
			t.Errorf("ordinary/unsupported route called: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := NewSessionHTTPTransport(server.Client(), server.URL+"/editing", fixtureReference(), func(context.Context) (string, string, error) { return strings.Repeat("d", 40), lease.BaseFileID, nil })
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	work := filepath.Join(directory, "source.dwg")
	os.WriteFile(work, []byte("approved content"), 0600)
	captured, err := CaptureSnapshot(work, directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	journal := FileJournal{Path: filepath.Join(directory, "journal.json")}
	workflow, err := New(State{Lease: lease}, client, journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = workflow.CommitCaptured(context.Background(), captured, false); err == nil {
		t.Fatal("lost acknowledgement treated as known success")
	}
	os.WriteFile(work, []byte("later unapproved work"), 0600)
	state, err := journal.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending.SourceIdentity != captured.SourceIdentity || state.Pending.SnapshotSize != captured.Size || state.Pending.SnapshotCreatedAt == "" {
		t.Fatal("snapshot consent not durable")
	}
	recovered, _ := New(state, client, journal)
	if err = recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publishes != 1 || recovered.State().Closed || string(uploaded) != "approved content" {
		t.Fatal("recovery republished or changed approved snapshot")
	}
	if err = recovered.CheckinUnchanged(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !recovered.State().Closed || recovered.State().Lease.Token != "" {
		t.Fatal("checkin retained active local credential")
	}
	retained, _ := os.ReadFile(work)
	if string(retained) != "later unapproved work" {
		t.Fatal("working copy was overwritten")
	}
}
func TestHTTPChangedSnapshotStopsBeforeUploadAndRedirectNeverForwardsProof(t *testing.T) {
	uploads := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads++; http.Redirect(w, r, "/other", 307) }))
	defer server.Close()
	remote, err := NewSessionHTTPTransport(server.Client(), server.URL+"/editing", fixtureReference(), func(context.Context) (string, string, error) {
		return strings.Repeat("d", 40), strings.Repeat("b", 40), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "capture.ready")
	os.WriteFile(file, []byte("changed"), 0400)
	intent := Intent{ID: "33333333-3333-4333-8333-333333333333", Snapshot: file, Digest: strings.Repeat("e", 64), ExpectedFileID: strings.Repeat("b", 40), Action: "commit"}
	if _, err = remote.Publish(context.Background(), fixtureLease(), intent); err == nil || uploads != 0 {
		t.Fatal("mutated snapshot sent")
	}
	if err = remote.Heartbeat(context.Background(), fixtureLease()); err == nil || uploads != 1 {
		t.Fatal("proof followed redirect")
	}
	if _, err = NewSessionHTTPTransport(server.Client(), "http://example.test/editing", fixtureReference(), remote.version); err == nil {
		t.Fatal("insecure endpoint accepted")
	}
}

func TestGuardedDownloadDoesNotForwardSessionOrOverwriteWorkingCopy(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "drawing.dwg")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/extensions/identity/v1/read-tickets/":
			cookie, err := r.Cookie("native-session")
			if err != nil || cookie.Value != "private" {
				t.Error("missing authenticated fixture session")
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"ticket": "11111111-1111-4111-8111-111111111111"})
		case "/seafhttp/cloudfile/read":
			if r.Header.Get("Cookie") != "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("incorrect download authority")
			}
			w.Write([]byte("native bytes"))
		default:
			t.Error("unexpected download route")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	origin, _ := url.Parse(server.URL)
	client.Jar.SetCookies(origin, []*http.Cookie{{Name: "native-session", Value: "private", Path: "/"}})
	remote, err := NewSessionHTTPTransport(client, server.URL+"/editing", fixtureReference(), func(context.Context) (string, string, error) {
		return strings.Repeat("d", 40), strings.Repeat("b", 40), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	download := func() error {
		return remote.DownloadWorkingCopy(context.Background(), fixtureLease(), server.URL+"/extensions/identity/v1/read-tickets/", server.URL+"/seafhttp/cloudfile/read", destination, 1024)
	}
	if err = download(); err != nil {
		t.Fatal(err)
	}
	if err = download(); err == nil {
		t.Fatal("overwrote unique local working copy")
	}
	bytes, _ := os.ReadFile(destination)
	if string(bytes) != "native bytes" {
		t.Fatal("working copy lost")
	}
}
