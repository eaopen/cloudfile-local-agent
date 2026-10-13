package config

import "testing"

// The preference order lives entirely in selectRoot, so a fake probe is enough
// to pin it without depending on which drives the test machine happens to have.
func TestSelectRootKeepsPreferenceOrderAndFallsBack(t *testing.T) {
	preferred := []string{`D:\CloudFileLocal\sessions`, `E:\CloudFileLocal\sessions`}
	fallback := `C:\Users\me\AppData\Local\CloudFileLocal\sessions`

	none := func(string) bool { return false }
	if got := selectRoot(preferred, fallback, none); got != fallback {
		t.Fatalf("no usable candidate must fall back to the system drive, got %q", got)
	}

	second := func(path string) bool { return path == preferred[1] }
	if got := selectRoot(preferred, fallback, second); got != preferred[1] {
		t.Fatalf("first usable candidate must win, got %q", got)
	}

	all := func(string) bool { return true }
	if got := selectRoot(preferred, fallback, all); got != preferred[0] {
		t.Fatalf("earlier candidates must be preferred over later ones, got %q", got)
	}
}

// No candidates means no probing, which is what macOS and Linux rely on.
func TestSelectRootWithoutCandidatesUsesFallback(t *testing.T) {
	fallback := "/home/me/.cache/CloudFileLocal/sessions"
	called := false
	probe := func(string) bool { called = true; return true }
	if got := selectRoot(nil, fallback, probe); got != fallback {
		t.Fatalf("got %q, want the fallback", got)
	}
	if called {
		t.Fatal("an empty candidate list must not probe anything")
	}
}
