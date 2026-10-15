package main

import (
	"path/filepath"
	"testing"
)

// isolateUserConfig redirects every platform's user-config location into a
// fresh temporary directory and returns that directory.
//
// os.UserConfigDir() resolves differently per platform - %AppData% on Windows,
// $HOME/Library/Application Support on macOS, $XDG_CONFIG_HOME (or
// $HOME/.config) elsewhere - so tests that only set HOME/XDG_CONFIG_HOME still
// wrote to the developer's real profile on Windows (a stray
// %AppData%\CloudFileAgent2\config.json whose workspace_root pointed at a
// deleted Temp path).
//
// Every test that reads or writes config.Config, or any other
// CloudFileAgent2 state kept under the user config dir, must call this instead
// of setting the environment variables by hand.
func isolateUserConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "appdata", "Local"))
	return home
}
