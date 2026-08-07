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
