package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// layoutRevision identifies the current on-disk shape. Roots written by an
// earlier revision are reorganised once and then stamped.
//
//	1  {root}/{repo_id}/{path}
//	2  {root}/{view|edit}/{repo_id}/{path}
//	3  {root}/{view|edit}/{repo_id}/{name}   flat, name looked up by record
const layoutRevision = "3"

// repoIDPattern matches the library and session identifiers the Hub issues
// (36-character hex UUIDs). Only such directories are treated as a legacy
// mirror, so an unrelated folder a user dropped in the root is left alone.
var repoIDPattern = regexp.MustCompile(`^[-0-9a-fA-F]{36}$`)

// Ensure makes root ready for the current layout revision: it adopts a legacy
// mirror (files written before the view/edit split) and flattens the nested
// library folders, then stamps the root so the work happens once. It is safe to
// call on every run.
func Ensure(root string) error {
	if root == "" {
		return fmt.Errorf("workspace root is empty")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if markerRevision(root) == layoutRevision {
		return nil
	}
	if err := adoptLegacyMirrors(root); err != nil {
		return err
	}
	if err := flattenMirrors(root); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, markerFile), []byte(layoutRevision+"\n"), 0600)
}

// markerRevision returns the layout revision a root was stamped with, or "".
func markerRevision(root string) string {
	data, err := os.ReadFile(filepath.Join(root, markerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// adoptLegacyMirrors moves files written at {root}/{repo_id}/{path} into the
// mode subtrees.
//
// The session mode was never recorded on disk, so the read-only marker is used
// to tell the copies apart: a view download is chmod 0400 and an edit download
// is writable. Anything that is not provably a view copy goes to the edit
// subtree, because keeping a disposable replica by mistake costs one download
// while discarding a user's work costs the work itself.
func adoptLegacyMirrors(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !repoIDPattern.MatchString(entry.Name()) {
			continue
		}
		if err := adoptLegacyRepo(root, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func adoptLegacyRepo(root, repoID string) error {
	legacy := filepath.Join(root, repoID)
	var sources []string
	err := filepath.WalkDir(legacy, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		sources = append(sources, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, source := range sources {
		rel, err := filepath.Rel(legacy, source)
		if err != nil {
			return err
		}
		sub := EditDir
		if info, err := os.Lstat(source); err == nil && IsReadOnly(info) {
			sub = ViewDir
		}
		target := filepath.Join(root, sub, repoID, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if _, err := os.Stat(target); err == nil {
			identical, err := sameContent(target, source)
			if err != nil {
				return err
			}
			if identical {
				if err := forceRemove(source); err != nil {
					return err
				}
				continue
			}
			// Both copies exist and differ. Keep both rather than picking one.
			target += legacySuffix
		}
		if err := os.Rename(source, target); err != nil {
			return err
		}
	}
	return removeTree(legacy)
}

// flattenMirrors turns a revision-2 nested mirror into the flat one:
//
//	{root}/edit/{repo}/design/plan/x.dwg -> {root}/edit/{repo}/x.dwg
//
// A name that is already taken becomes "x (1).dwg", and each moved file's
// fingerprint is re-pointed at its new name so the in-library path keeps
// resolving to the local copy. Nothing is discarded: a name clash only ever
// adds a suffix.
func flattenMirrors(root string) error {
	// The subtree name ("view"/"edit") is not the session mode ("local-view"/
	// "local-edit"); the fingerprints are keyed by the mode, so both travel
	// together here.
	for _, subtree := range []struct{ dir, mode string }{
		{ViewDir, ModeView},
		{EditDir, ModeEdit},
	} {
		base := filepath.Join(root, subtree.dir)
		entries, err := os.ReadDir(base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if err := flattenRepo(root, subtree.dir, subtree.mode, entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func flattenRepo(root, subtree, mode, repoID string) error {
	dir := filepath.Join(root, subtree, repoID)
	var sources []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		sources = append(sources, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, source := range sources {
		rel, err := filepath.Rel(dir, source)
		if err != nil {
			return err
		}
		serverPath := "/" + filepath.ToSlash(rel)
		name := filepath.Base(rel)
		if filepath.Dir(rel) == "." {
			// Already flat; only the fingerprint may need the name.
			if err := stampRecordName(root, mode, repoID, serverPath, name); err != nil {
				return err
			}
			continue
		}
		unique, err := UniqueName(dir, name)
		if err != nil {
			return err
		}
		if err := os.Rename(source, filepath.Join(dir, unique)); err != nil {
			return err
		}
		if err := stampRecordName(root, mode, repoID, serverPath, unique); err != nil {
			return err
		}
	}
	return pruneEmptyDirs(dir)
}

// stampRecordName records which file name a mirrored file now carries. A file
// whose fingerprint is missing gets a name-only record: it is deliberately left
// without a digest or file id, so a view copy is re-fetched rather than trusted
// while an edit copy is still treated as "a local copy exists".
func stampRecordName(root, mode, repoID, serverPath, name string) error {
	record, found := ReadRecord(root, mode, repoID, serverPath)
	if found && record.Name == name {
		return nil
	}
	record.Name = name
	record.RepoID = repoID
	record.Path = serverPath
	record.Mode = mode
	return WriteRecord(root, mode, repoID, serverPath, record)
}

// pruneEmptyDirs removes the directories the flattened files left behind,
// deepest first. A directory that still holds anything is kept.
func pruneEmptyDirs(dir string) error {
	var directories []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != dir {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Remove(directories[index]); err != nil && !os.IsNotExist(err) {
			// A non-empty directory simply still holds files written by someone
			// else; that is not a migration failure.
			if entries, readErr := os.ReadDir(directories[index]); readErr != nil || len(entries) > 0 {
				continue
			}
			return err
		}
	}
	return nil
}

// Migrate moves the whole mirror tree from the previous default location to the
// current one, so an existing mirror follows the layout policy onto another
// drive. Both subtrees and the fingerprints travel as one unit.
//
// Nothing is merged: if the destination already holds a mirror the move is
// skipped, because interleaving two mirrors could overwrite local edits.
func Migrate(previous, current string) error {
	if previous == "" || current == "" {
		return nil
	}
	from, err := filepath.Abs(previous)
	if err != nil {
		return err
	}
	to, err := filepath.Abs(current)
	if err != nil {
		return err
	}
	if strings.EqualFold(from, to) {
		return nil
	}
	source, err := hasMirrorContent(from)
	if err != nil || !source {
		return err
	}
	target, err := hasMirrorContent(to)
	if err != nil {
		return err
	}
	if target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	return moveTree(from, to)
}

// hasMirrorContent reports whether a directory holds anything worth migrating.
// A bare layout marker does not count: Ensure may already have stamped a fresh
// destination root, and that must not block the move.
func hasMirrorContent(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == markerFile {
			continue
		}
		return true, nil
	}
	return false, nil
}

// moveTree renames the tree when both ends share a volume, and otherwise copies
// it to a staging directory that is renamed into place before the source is
// removed. The source is only ever deleted after the destination is complete,
// so a failure mid-copy leaves the original mirror untouched.
func moveTree(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	staging := to + ".moving"
	if err := removeTree(staging); err != nil {
		return err
	}
	if err := copyTree(from, staging); err != nil {
		_ = removeTree(staging)
		return err
	}
	// The destination may already exist as a bare stamped root (Ensure runs
	// before Migrate). It holds no mirror content by definition -- an occupied
	// target was rejected earlier -- so the whole tree is replaced, marker
	// included; a plain os.Remove would fail on the marker file.
	if info, err := os.Stat(to); err == nil && info.IsDir() {
		if err := removeTree(to); err != nil {
			_ = removeTree(staging)
			return err
		}
	}
	if err := os.Rename(staging, to); err != nil {
		_ = removeTree(staging)
		return err
	}
	return removeTree(from)
}

// removeTree deletes a mirror tree. Windows refuses to delete a file that still
// carries the read-only attribute, and every view replica does, so the write
// bit is restored before the tree is removed.
func removeTree(root string) error {
	if _, err := os.Lstat(root); err != nil {
		return nil
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		_ = os.Chmod(path, 0600)
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// forceRemove deletes one file, clearing the read-only attribute first.
func forceRemove(path string) error {
	if err := os.Remove(path); err == nil {
		return nil
	}
	_ = os.Chmod(path, 0600)
	return os.Remove(path)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// A link or device could point anywhere; refuse the move instead of
			// following it and copying something unintended.
			return fmt.Errorf("mirror contains an unsupported entry: %s", path)
		}
		return copyFile(path, target, info)
	})
}

func copyFile(from, to string, info os.FileInfo) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	target, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		_ = os.Remove(to)
		return err
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(to)
		return err
	}
	if err := os.Chmod(to, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(to, info.ModTime(), info.ModTime())
}
