package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadAndClaimV2EditSession(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2.1/cloudfile/agent-sessions/claim/" {
			http.NotFound(w, r)
			return
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["ticket"] != "one-time" {
			t.Fatalf("unexpected ticket: %q", request["ticket"])
		}
		_ = json.NewEncoder(w).Encode(Claimed{
			SessionID: "66c8165f-c27d-4adb-b897-8abaf05e7614",
			Mode:      "local-edit",
			ExpiresAt: time.Now().Add(time.Minute).Unix(),
			File: File{Name: "plan.docx",
				ContentURL: server.URL + "/content"},
			Writeback: &Writeback{
				ContentURL:   server.URL + "/writeback",
				HeartbeatURL: server.URL + "/heartbeat",
				Capability:   "capability",
			},
		})
	}))
	defer server.Close()

	descriptor := Descriptor{
		Protocol: Protocol, Server: server.URL, Ticket: "one-time",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	path := filepath.Join(t.TempDir(), "plan.cloudfile")
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	read, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := Claim(read)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Mode != "local-edit" || claimed.Writeback == nil ||
		claimed.Writeback.Capability != "capability" {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
}

func TestClaimRejectsCrossOriginCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Claimed{
			SessionID: "session", Mode: "local-view",
			ExpiresAt: time.Now().Add(time.Minute).Unix(),
			File:      File{Name: "plan.docx", ContentURL: "https://attacker.invalid/file"},
		})
	}))
	defer server.Close()

	_, err := Claim(Descriptor{
		Protocol: Protocol, Server: server.URL, Ticket: "ticket",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err == nil || !strings.Contains(err.Error(), "untrusted URL") {
		t.Fatalf("expected cross-origin refusal, got %v", err)
	}
}

func TestReadRejectsExpiredDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expired.cloudfile")
	data, err := json.Marshal(Descriptor{
		Protocol: Protocol, Server: "https://cloudfile.example", Ticket: "ticket",
		ExpiresAt: time.Now().Add(-time.Second).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry refusal, got %v", err)
	}
}
