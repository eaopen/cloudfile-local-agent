package runner

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

func Run(path string) error {
	descriptor, err := session.Read(path)
	if err != nil {
		return err
	}
	return RunDescriptor(descriptor)
}

// RunDescriptor drives a full session from an already-parsed descriptor.  It is
// shared by the file path (Run) and the extension-message path, so the security
// gates (origin allow-list, one-time ticket claim) are identical for both.
func RunDescriptor(descriptor session.Descriptor) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Allows(descriptor.Server) {
		return fmt.Errorf("the CloudFile origin is not trusted by this agent")
	}
	claimed, err := session.Claim(descriptor)
	if err != nil {
		return err
	}
	root, err := workspace.Resolve(cfg)
	if err != nil {
		return err
	}
	name := filepath.Base(claimed.File.Name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return fmt.Errorf("session returned an invalid file name")
	}
	if workspace.RelPath(claimed.Path) == "" {
		return fmt.Errorf("session returned an invalid file path")
	}
	repoID := RepoID(claimed)
	dir, err := workspace.Dir(root, claimed.Mode, repoID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	localPath, err := materialize(root, claimed, repoID, dir, descriptor.LocalAction)
	if err != nil {
		return err
	}
	return openFile(cfg, claimed.Mode, localPath)
}

// RepoID is the library folder that keys the mirror subtree. The session
// identifier is only a fallback: a library keeps its folder when it is renamed
// or moved, so mirroring by repo_id avoids needless re-downloads.
func RepoID(claimed session.Claimed) string {
	if claimed.RepoID != "" {
		return claimed.RepoID
	}
	return claimed.SessionID
}

// materialize makes sure the local copy this session should open exists, and
// returns its path.
//
// The library folder is flat and a file is identified by its in-library path,
// so the on-disk name comes from the download record (it may carry a " (1)"
// suffix because another library path already owns the plain name). Only a file
// that has never been downloaded gets a freshly reserved name.
//
// View copies and edit copies never share a folder, so opening a file for
// viewing can no longer overwrite a half-finished local edit. Within a folder:
//
//   - view: the replica is reused only while it still matches both the
//     fingerprint recorded at download time and the version the server serves
//     now. Anything else re-fetches it into the same name, which is cheap
//     because a replica is disposable.
//   - edit: the working copy is never touched unless the caller explicitly
//     asked to overwrite it. Local work is only discarded on an explicit
//     decision, never as a side effect.
//
// An unrecognised action falls back to keeping the local copy: refusing to
// destroy local work costs at most one stale file, while getting it wrong costs
// the work itself.
func materialize(root string, claimed session.Claimed, repoID, dir, action string) (string, error) {
	name, recorded := workspace.FileName(root, claimed.Mode, repoID, claimed.Path)
	if name == "" {
		return "", fmt.Errorf("session returned an unusable file name")
	}
	localPath := filepath.Join(dir, name)
	if recorded {
		if info, err := os.Stat(localPath); err == nil && info.Mode().IsRegular() {
			if claimed.Mode == workspace.ModeView {
				if unchanged(root, claimed, repoID, localPath) {
					return localPath, nil
				}
				return localPath, fetch(root, claimed, repoID, localPath, name)
			}
			if action == "overwrite" {
				return localPath, fetch(root, claimed, repoID, localPath, name)
			}
			return localPath, nil
		}
		// The recorded copy is gone (moved or deleted outside the agent).
		// Downloading again under the same name keeps the fingerprint valid.
		return localPath, fetch(root, claimed, repoID, localPath, name)
	}
	// No record: this library path has never been downloaded. Its plain name may
	// already belong to a different library path, so a free one is reserved the
	// way Windows would ("name (1)", "name (2)", ...).
	unique, err := workspace.UniqueName(dir, name)
	if err != nil {
		return "", err
	}
	localPath = filepath.Join(dir, unique)
	return localPath, fetch(root, claimed, repoID, localPath, unique)
}

// unchanged reports whether a local replica still matches the fingerprint
// recorded when it was downloaded and the version the server is serving now.
// A missing or partial fingerprint answers "no", so the replica is re-fetched
// rather than trusted.
func unchanged(root string, claimed session.Claimed, repoID, localPath string) bool {
	record, found := workspace.ReadRecord(root, claimed.Mode, repoID, claimed.Path)
	if !found || record.Digest == "" || record.FileID == "" || claimed.FileID == "" {
		return false
	}
	if record.FileID != claimed.FileID {
		return false
	}
	digest, err := workspace.DigestFile(localPath)
	return err == nil && digest == record.Digest
}

// fetch downloads the server version over localPath and records what was
// downloaded, including the name it was stored under.
//
// A view replica is made read-only, so a slip inside a viewer cannot silently
// turn it into a working copy. An edit copy is made writable, because the user
// is expected to save into it.
func fetch(root string, claimed session.Claimed, repoID, localPath, name string) error {
	if err := download(claimed.File.ContentURL, localPath); err != nil {
		return err
	}
	if claimed.Mode == workspace.ModeView {
		_ = os.Chmod(localPath, 0400)
	} else {
		_ = os.Chmod(localPath, 0600)
	}
	// The download already succeeded; a fingerprint that cannot be written only
	// costs the next open a re-check, so it never fails the session.
	digest, err := workspace.DigestFile(localPath)
	if err != nil {
		return nil
	}
	size := int64(0)
	if info, err := os.Stat(localPath); err == nil {
		size = info.Size()
	}
	_ = workspace.WriteRecord(root, claimed.Mode, repoID, claimed.Path, workspace.Record{
		RepoID: repoID,
		Path:   claimed.Path,
		Mode:   claimed.Mode,
		Name:   name,
		FileID: claimed.FileID,
		Digest: digest,
		Size:   size,
	})
	return nil
}

// download writes rawURL to target, replacing any existing file.
func download(rawURL, target string) error {
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Get(rawURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("file download failed (%d)", response.StatusCode)
	}
	// Windows refuses to open a read-only file for writing, and a previous view
	// download leaves its replica read-only, so the bit is cleared before the
	// truncating open rather than after it fails.
	if _, err := os.Stat(target); err == nil {
		_ = os.Chmod(target, 0600)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	const maxDownloadBytes = 4 * 1024 * 1024 * 1024
	written, err := io.Copy(file, io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil {
		_ = os.Remove(target)
		return err
	}
	if written > maxDownloadBytes {
		_ = os.Remove(target)
		return fmt.Errorf("file exceeds the local download limit")
	}
	return nil
}

func openFile(agentConfig config.Config, mode, path string) error {
	if rule, found := agentConfig.ResolveOpenRule(mode, filepath.Base(path)); found {
		return launch(rule.Command, path)
	}
	if application, found := appfinder.ForFile(filepath.Base(path)); found {
		return launch(application.Command, path)
	}
	return openDefault(path)
}

func launch(command []string, path string) error {
	arguments := make([]string, len(command)-1)
	for index, argument := range command[1:] {
		arguments[index] = strings.ReplaceAll(argument, "{file}", path)
	}
	return exec.Command(command[0], arguments...).Start()
}

func openDefault(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", path)
	case "darwin":
		command = exec.Command("open", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}
