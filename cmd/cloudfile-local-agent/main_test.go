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
	want := map[string]bool{"open_session": true, "open_session_file": true, "query_local_file": true, "query_local_folder": true, "open_workspace": true, "check_update": true, "open_uri": true}
	for _, cap := range got.Capabilities {
		if !want[cap] {
			t.Fatalf("advertised unsupported capability %s", cap)
		}
		delete(want, cap)
	}
	if !containsCapabilities(got.Capabilities, "check_update") {
		t.Fatal("update capability missing")
	}
	_ = want // not all capabilities are advertised until Go is paired
}

func containsCapabilities(capabilities []string, name string) bool {
	for _, capability := range capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

func TestAgent2DoesNotStealUnconfiguredWindowsSessions(t *testing.T) {
	unpaired := advertisedCapabilities(nil)
	if containsCapabilities(unpaired, "open_session") || containsCapabilities(unpaired, "open_workspace") {
		t.Fatalf("unconfigured Go agent took over old Windows operations: %v", unpaired)
	}
	paired := advertisedCapabilities([]string{"https://cloudfile.example"})
	if !containsCapabilities(paired, "open_session") || !containsCapabilities(paired, "query_local_file") {
		t.Fatalf("paired Go agent omitted available operations: %v", paired)
	}
}

func TestMacOpenURIIsAdvertisedOnlyForDarwin(t *testing.T) {
	got := advertisedCapabilities(nil)
	if containsCapabilities(got, "open_uri") != (runtime.GOOS == "darwin") {
		t.Fatalf("platform-specific paired URI capability incorrect: %v", got)
	}
}
