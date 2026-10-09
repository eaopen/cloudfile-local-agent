package nativehost

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestServeUsesChromeLengthPrefixedMessages(t *testing.T) {
	payload, err := json.Marshal(Request{Type: "status"})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := binary.Write(&input, binary.LittleEndian, uint32(len(payload))); err != nil {
		t.Fatal(err)
	}
	input.Write(payload)
	var output bytes.Buffer
	if err := Serve(&input, &output, func(request Request) Response {
		if request.Type != "status" {
			t.Fatalf("got %q", request.Type)
		}
		return Response{OK: true, Version: "test"}
	}); err != nil {
		t.Fatal(err)
	}
	var length uint32
	if err := binary.Read(&output, binary.LittleEndian, &length); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, length)
	if _, err := output.Read(result); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Version != "test" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestRequestOnlyAcceptsCloudFileSessionPaths(t *testing.T) {
	if !(Request{Type: "open_session_file", Path: `C:\\Downloads\\plan.cloudfile`}).Valid() {
		t.Fatal("expected a session file request to be valid")
	}
	if (Request{Type: "open_session_file", Path: `C:\\Downloads\\plan.txt`}).Valid() {
		t.Fatal("non-session files must be rejected")
	}
}

// check_update is the extension's polling probe. Like status it is a bare
// version query: it carries no payload, so a path is rejected. (Other unused
// fields are ignored, matching the existing status/update contract.)
func TestCheckUpdateRequestValidity(t *testing.T) {
	if !(Request{Type: "check_update"}).Valid() {
		t.Fatal("a bare check_update request must be valid")
	}
	if (Request{Type: "check_update", Path: "x"}).Valid() {
		t.Fatal("check_update with a path must be rejected")
	}
}

// update_extension asks the agent to install a new extension package on disk;
// like update it carries no payload.
func TestUpdateExtensionRequestValidity(t *testing.T) {
	if !(Request{Type: "update_extension"}).Valid() {
		t.Fatal("a bare update_extension request must be valid")
	}
	if (Request{Type: "update_extension", Path: "x"}).Valid() {
		t.Fatal("update_extension with a path must be rejected")
	}
}

// query_local_folder is a per-library question: it needs a library id and,
// unlike query_local_file, it must NOT carry a path (it addresses the folder).
func TestQueryLocalFolderRequestValidity(t *testing.T) {
	if !(Request{Type: "query_local_folder", RepoID: "repo-1"}).Valid() {
		t.Fatal("a library-only folder query must be valid")
	}
	if (Request{Type: "query_local_folder"}).Valid() {
		t.Fatal("a folder query without a library must be rejected")
	}
	if (Request{Type: "query_local_folder", RepoID: "repo-1", Ticket: "t"}).Valid() {
		t.Fatal("a folder query must not carry session credentials")
	}
}

// An unknown message type must never reach the handler: Valid() is the only
// thing standing between a malformed message and the switch in main.go.
func TestUnknownTypeIsRejected(t *testing.T) {
	if (Request{Type: "update_extension2"}).Valid() {
		t.Fatal("an unknown message type must be rejected")
	}
	if (Request{Type: ""}).Valid() {
		t.Fatal("an empty message type must be rejected")
	}
}
