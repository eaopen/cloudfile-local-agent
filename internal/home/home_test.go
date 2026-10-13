package home

import (
	"os"
	"path/filepath"
	"testing"
)

var testRoots = []string{`D:\CloudFileLocal`, `C:\CloudFileLocal`}

const testFallback = `C:\Users\someone\AppData\Local\CloudFileLocal`

func none(string) bool { return false }

func all(string) bool { return true }

// The manifest is the anchor: it is the install the Chrome registry points at,
// so a version that merely prefers another drive must not abandon it.
func TestSelectDirPrefersTheRegisteredInstall(t *testing.T) {
	got := selectDir("", testRoots, testFallback,
		func(base string) bool { return base == testFallback },
		all)
	if got != testFallback {
		t.Fatalf("an existing install must be followed, got %s", got)
	}

	// And the drive preference loses to it even when the drive looks perfect.
	got = selectDir("", testRoots, testFallback,
		func(base string) bool { return base == testRoots[1] },
		func(base string) bool { return base == testRoots[0] })
	if got != testRoots[1] {
		t.Fatalf("the registered install must win over the preferred drive, got %s", got)
	}
}

func TestSelectDirFallsToThePreferredDrive(t *testing.T) {
	cases := []struct {
		usable func(string) bool
		want   string
	}{
		{func(base string) bool { return base == testRoots[0] }, testRoots[0]},
		// D: is missing (or is an optical/mapped drive): C: takes over, which
		// is what "no D drive, use C" means to the installer.
		{func(base string) bool { return base == testRoots[1] }, testRoots[1]},
		// Neither root usable: the per-user folder keeps the product working.
		{none, testFallback},
	}
	for _, c := range cases {
		if got := selectDir("", testRoots, testFallback, none, c.usable); got != c.want {
			t.Fatalf("selectDir = %s, want %s", got, c.want)
		}
	}
}

func TestSelectDirHonoursTheOverride(t *testing.T) {
	got := selectDir(`D:\Other\`, testRoots, testFallback, all, all)
	if want := filepath.Clean(`D:\Other\`); got != want {
		t.Fatalf("override = %s, want %s", got, want)
	}
}

func TestDirHonoursTheEnvironmentOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envOverride, dir)
	if got := Dir(); got != filepath.Clean(dir) {
		t.Fatalf("Dir = %s, want %s", got, dir)
	}
	// A padded value is still the same folder.
	t.Setenv(envOverride, "  "+dir+"  ")
	if got := Dir(); got != filepath.Clean(dir) {
		t.Fatalf("Dir = %q, want %s", got, dir)
	}
}

func TestWritableCreatesAndCleansUpAfterItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "CloudFileLocal")
	if !Writable(dir) {
		t.Fatal("a fresh folder under a writable parent must be usable")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the write probe was left behind: %v", entries)
	}
}

func TestWritableRejectsAPathUnderAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if Writable(filepath.Join(file, "child")) {
		t.Fatal("a folder inside a file must not be reported usable")
	}
}
