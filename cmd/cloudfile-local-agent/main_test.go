package main

import (
	"runtime"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
)

func TestAgent2StatusDeclaresOnlyImplementedCapabilities(t *testing.T) {
	got := handleNativeMessage(nativehost.Request{Type: "status"})
	if !got.OK || got.Version != "2.0.1" || got.AgentFamily != "agent2" || got.Platform != runtime.GOOS {
		t.Fatalf("invalid Agent2 status family/version/platform: %+v", got)
	}
	want := map[string]bool{"open_session": true, "open_session_file": true, "query_local_file": true, "query_local_folder": true, "open_workspace": true, "check_update": true}
	for _, cap := range got.Capabilities {
		if !want[cap] {
			t.Fatalf("advertised unsupported capability %s", cap)
		}
		delete(want, cap)
	}
	if len(want) != 0 {
		t.Fatalf("missing basic capabilities: %v", want)
	}
}
