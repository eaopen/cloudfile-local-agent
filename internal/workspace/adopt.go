package workspace

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
)

// mirrorSubtrees are the only directories a mirror consists of.  Anything else
// sitting in the source directory belongs to somebody else and is never
// touched, copied or removed.
var mirrorSubtrees = []string{"view", "edit", ".cloudfile-records"}

// adoptReportName is written inside the mirror after a move so a support
// engineer can see what happened without a console.
const adoptReportName = "mirror-adopt.json"

type adoptOutcome struct {
	From    string `json:"from"`
	To      string `json:"to"`
	At      string `json:"at"`
	State   string `json:"state"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped"`
	Missing int    `json:"missing"`
	Detail  string `json:"detail,omitempty"`
}

// Adopt makes root the mirror's home, carrying an existing mirror over once.
//
// A machine can resolve a different root than the one it used last time:
// D:\CloudFileAgent2\workspace drops out (drive removed, read-only, full) and
// the fallback under the user config directory takes over, or the user edits
// workspace_root.  Re-downloading is not a fix for edit copies -- they are the
// user's only copy of local work -- so the mirror is moved instead.
//
// Rules, in order of importance:
//
//  1. Nothing is removed before the copy is complete and verified.
//  2. An existing file in the target is never overwritten, and a target that
//     already holds mirror content is never merged: it wins, the source stays
//     where it is, and the marker moves on.  Silent merging of two mirrors
//     would be guessing which copy is newer.
//  3. Only view/, edit/ and .cloudfile-records/ are considered.
//  4. Symlinks and irregular files are skipped, never followed.
//
// A change is noticed at most once: the marker is written even when the move
// is impossible, so a flapping drive cannot retry a multi-gigabyte copy on
// every call.  The returned error is informational -- callers must never fail
// a session because of it, since the worst case is that files stayed put.
func Adopt(root string) (bool, error) {
	if root == "" || !filepath.IsAbs(root) {
		return false, errors.New("absolute mirror root required")
	}

	// Fast path: one small file read once the mirror has settled.  This runs on
	// every resolution, so it must stay cheap.
	if samePath(config.RecordedMirrorRoot(), root) {
		return false, nil
	}

	source := adoptSource(root)
	if source == "" {
		return false, config.RecordMirrorRoot(root)
	}

	files, bytes := mirrorSize(source)
	if files == 0 {
		// Nothing worth carrying: just remember the new home.
		return false, config.RecordMirrorRoot(root)
	}
	if occupied, _ := mirrorSize(root); occupied > 0 {
		return false, reportAdopt(root, adoptOutcome{From: source, To: root,
			State: "skipped", Files: files, Bytes: bytes,
			Detail: "target already holds mirror content; source left in place"})
	}

	copied, skipped, err := copyMirror(source, root)
	outcome := adoptOutcome{From: source, To: root, Files: copied, Bytes: bytes, Skipped: skipped}
	if err != nil {
		outcome.State, outcome.Detail = "failed", err.Error()
		return true, reportAdopt(root, outcome)
	}
	missing, err := verifyMirror(source, root)
	if err != nil || missing > 0 {
		outcome.State, outcome.Missing = "partial", missing
		if err != nil {
			outcome.Detail = err.Error()
		} else {
			outcome.Detail = "verification found missing or truncated files; source kept"
		}
		return true, reportAdopt(root, outcome)
	}
	// Verified byte-for-byte, so the source may go.  A failure here only leaves
	// a duplicate behind; the mirror itself is already complete and correct.
	outcome.State = "done"
	if err = removeMirror(source); err != nil {
		outcome.State, outcome.Detail = "copied", "source could not be removed: "+err.Error()
	}
	return true, reportAdopt(root, outcome)
}

// adoptSource picks where to carry the mirror from: whatever the marker names,
// then the current platform default, then the defaults used before the drive
// policy settled.  A candidate that actually holds files wins over one that is
// merely empty, and ties fall to the earliest candidate so a fresh machine
// still records its root instead of scanning forever.
func adoptSource(root string) string {
	guess := ""
	for _, candidate := range adoptCandidates() {
		if candidate == "" || samePath(candidate, root) {
			continue
		}
		if guess == "" {
			guess = candidate
		}
		if files, _ := mirrorSize(candidate); files > 0 {
			return candidate
		}
	}
	return guess
}

func adoptCandidates() []string {
	candidates := []string{config.RecordedMirrorRoot()}
	if fallback, err := config.FallbackRoot(); err == nil {
		candidates = append(candidates, fallback)
	}
	return append(candidates, config.SupersededFallbackRoots()...)
}

// samePath compares two absolute paths, folding case on Windows where the file
// system does.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left, right := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// mirrorSize counts the regular files (and their bytes) that make up a mirror.
// Unreadable or absent directories count as empty: an unplugged drive or a
// first run must not look like an error.
func mirrorSize(root string) (int, int64) {
	var files int
	var bytes int64
	for _, sub := range mirrorSubtrees {
		_ = filepath.WalkDir(filepath.Join(root, sub), func(_ string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.Type().IsRegular() {
				if info, err := entry.Info(); err == nil {
					files++
					bytes += info.Size()
				}
			}
			return nil
		})
	}
	return files, bytes
}

// copyMirror copies the mirror subtrees from source to target without ever
// overwriting an existing target file.  It returns how many files were copied
// and how many were left alone because the target already had that name.
func copyMirror(source, target string) (int, int, error) {
	copied, skipped := 0, 0
	for _, sub := range mirrorSubtrees {
		from := filepath.Join(source, sub)
		if err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			to := filepath.Join(target, relative)
			if entry.IsDir() {
				return os.MkdirAll(to, 0700)
			}
			if !entry.Type().IsRegular() {
				// Symlinks, devices and sockets are never followed or recreated.
				return nil
			}
			if _, err := os.Lstat(to); err == nil {
				skipped++
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
				return err
			}
			if err := copyFile(path, to); err != nil {
				return err
			}
			copied++
			return nil
		}); err != nil && !os.IsNotExist(err) {
			return copied, skipped, err
		}
	}
	return copied, skipped, nil
}

func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	// Created writable, then given the source's mode: a read-only view replica
	// must stay read-only, but Windows makes writing to a freshly created
	// read-only file awkward, so the bit is applied after the data lands.
	destination, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(destination, source); err != nil {
		destination.Close()
		os.Remove(to)
		return err
	}
	if err = destination.Sync(); err != nil {
		destination.Close()
		os.Remove(to)
		return err
	}
	if err = destination.Close(); err != nil {
		os.Remove(to)
		return err
	}
	return os.Chmod(to, info.Mode().Perm())
}

// verifyMirror reports how many source files are missing from the target or
// arrived with a different size.  Anything above zero keeps the source.
func verifyMirror(source, target string) (int, error) {
	missing := 0
	for _, sub := range mirrorSubtrees {
		err := filepath.WalkDir(filepath.Join(source, sub), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			copied, err := os.Lstat(filepath.Join(target, relative))
			if err != nil || !copied.Mode().IsRegular() || copied.Size() != info.Size() {
				missing++
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return missing, err
		}
	}
	return missing, nil
}

// removeMirror deletes the mirror subtrees of a source whose contents were
// verified in the target.  Only those three names are removed.
func removeMirror(source string) error {
	for _, sub := range mirrorSubtrees {
		if err := os.RemoveAll(filepath.Join(source, sub)); err != nil {
			return err
		}
	}
	return nil
}

// reportAdopt records the outcome next to the mirror data and moves the marker
// on, so the same change is never processed twice.
func reportAdopt(root string, outcome adoptOutcome) error {
	outcome.At = time.Now().Format(time.RFC3339)
	if data, err := json.MarshalIndent(outcome, "", "  "); err == nil {
		dir := filepath.Join(root, mirrorSubtrees[2])
		if os.MkdirAll(dir, 0700) == nil {
			_ = os.WriteFile(filepath.Join(dir, adoptReportName), append(data, '\n'), 0600)
		}
	}
	return config.RecordMirrorRoot(root)
}
