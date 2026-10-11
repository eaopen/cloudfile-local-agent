package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func componentRequest(body string) string { return `{"ipc_version":1,"request":` + body + `}` }
func TestComponentCapabilitiesAndRejectedOperations(t *testing.T) {
	var output bytes.Buffer
	if err := serveComponent(strings.NewReader(componentRequest(`{"type":"capabilities"}`)), &output); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK         bool           `json:"ok"`
		IPCVersion int            `json:"ipc_version"`
		Result     map[string]any `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	report := envelope.Result
	if !envelope.OK || envelope.IPCVersion != 1 || report["core_version"] != "2.0.1" || report["native_device_publish"] != false || report["automatic_upload"] != false {
		t.Fatalf("invalid report: %v", envelope)
	}
	for _, input := range []string{
		`{"type":"update"}`, `{"type":"update_extension"}`, `{"type":"open_session_file","path":"a.cloudfile"}`,
		`{"type":"capabilities","command":"evil"}`, `{"type":"capabilities","type":"status"}`,
		`{"type":"capabilities"} {}`, `null`, `[]`, `{"type":"query_local_file"}`,
		`{"type":"capabilities","ticket":"secret"}`,
	} {
		output.Reset()
		if err := serveComponent(strings.NewReader(componentRequest(input)), &output); err != nil {
			t.Fatal(err)
		}
		var rejected map[string]any
		json.Unmarshal(output.Bytes(), &rejected)
		if rejected["ok"] != false || rejected["error_code"] == nil {
			t.Fatalf("accepted unsafe input: %s", input)
		}
	}
	for _, request := range []string{`{"ipc_version":2,"request":{"type":"capabilities"}}`, `{"ipc_version":1,"ipc_version":1,"request":{"type":"capabilities"}}`, `{"type":"capabilities"}`} {
		output.Reset()
		serveComponent(strings.NewReader(request), &output)
		var rejected map[string]any
		json.Unmarshal(output.Bytes(), &rejected)
		if rejected["ok"] != false {
			t.Fatal("accepted incompatible or ambiguous protocol")
		}
	}
	output.Reset()
	if serveComponent(strings.NewReader(strings.Repeat(" ", componentBudget+1)), &output) == nil || output.Len() != 0 {
		t.Fatal("unbounded input accepted")
	}
}
