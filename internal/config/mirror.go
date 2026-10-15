package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// MirrorRootName is the marker that remembers where the local mirror lived the
// last time the agent resolved it.
//
// It deliberately sits next to config.json -- a location that does NOT move
// with the mirror -- so the mirror can be found again after the resolved root
// changes: "D: came back after being unavailable", or a hand-edited
// workspace_root.  Without it the agent would silently start from an empty
// mirror and the user's local edit copies would be left behind.
const MirrorRootName = "mirror-root.txt"

// MirrorRecordPath is the absolute path of that marker.
func MirrorRecordPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "CloudFileAgent2", MirrorRootName), nil
}

// RecordedMirrorRoot returns the last recorded mirror root, or "" when nothing
// was recorded.  A missing, unreadable or garbled marker is not an error: the
// worst case is one missed migration, which must never break a session.
func RecordedMirrorRoot() string {
	path, err := MirrorRecordPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(data))
	if root == "" || !filepath.IsAbs(root) {
		return ""
	}
	return root
}

// RecordMirrorRoot remembers root, using the same temp-file + rename dance as
// Save so a crash cannot leave a half-written path behind.
func RecordMirrorRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("absolute mirror root required")
	}
	path, err := MirrorRecordPath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mirror-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write([]byte(root + "\n")); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
