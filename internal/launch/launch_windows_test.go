//go:build windows

package launch

import "testing"

// TestCreationFlagsDetachFromCallersJob pins the Windows flags: a background
// upgrade must break out of the job object Chrome may hold the native host in,
// and must run without a console window.
func TestCreationFlagsDetachFromCallersJob(t *testing.T) {
	withBreakaway := creationFlags(true)
	if withBreakaway&createBreakawayFromJob == 0 {
		t.Fatal("the breakaway flag must be set when the job allows it")
	}
	if withBreakaway&detachedProcess == 0 {
		t.Fatal("the child must run detached (no console)")
	}
	if withBreakaway&createNewProcessGroup == 0 {
		t.Fatal("the child must not share the caller's process group")
	}

	fallback := creationFlags(false)
	if fallback&createBreakawayFromJob != 0 {
		t.Fatal("the fallback must drop the breakaway flag")
	}
	if fallback&detachedProcess == 0 {
		t.Fatal("the fallback must stay detached")
	}
}
