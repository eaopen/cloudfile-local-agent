package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestLocalUploadRejectsUnconfirmedAndMalformedInput(t *testing.T) {
	t.Setenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED", "1")
	for _, input := range []string{
		`{}`, `{"instance":"http://untrusted","user_confirmed":true}`,
		`{"action":"commit","action":"recover"}`, strings.Repeat(" ", componentBudget+1),
		`{"instance":"https://example.test","device_id":"11111111-1111-4111-8111-111111111111","session_id":"22222222-2222-4222-8222-222222222222","action":"commit","working_copy":"/tmp/file","user_confirmed":false}`,
	} {
		var output bytes.Buffer
		if runEditUpload(strings.NewReader(input), &output) == nil || output.Len() != 0 {
			t.Fatal("invalid local request accepted")
		}
	}
}
func TestLocalUploadDisabledWithoutExplicitGate(t *testing.T) {
	t.Setenv("CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED", "")
	if runEditUpload(strings.NewReader("{}"), &bytes.Buffer{}) == nil {
		t.Fatal("disabled uploader accepted")
	}
}
func TestRemoteComponentCannotNominateAnUpload(t *testing.T) {
	for _, op := range []string{"commit", "checkin", "recover", "edit-upload-stdin"} {
		if _, err := componentResult([]byte(`{"type":"` + op + `","working_copy":"/private/file","user_confirmed":true}`)); err == nil {
			t.Fatal("remote local-file nomination accepted")
		}
	}
}
