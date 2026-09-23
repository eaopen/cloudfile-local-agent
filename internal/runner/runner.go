package runner

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/fsnotify/fsnotify"
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
	root, err := cfg.Root()
	if err != nil {
		return err
	}
	name := filepath.Base(claimed.File.Name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return fmt.Errorf("session returned an invalid file name")
	}
	// Mirror the server directory layout under {root}/{repo_id}/{path},
	// with repo_id as the first level (stable across library rename/move).
	localPath, err := localMirrorPath(root, claimed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
		return err
	}

	// Reuse decision for non-view modes: if a local copy already exists and its
	// content hash differs from the server file_id, honour the caller's action.
	if claimed.Mode != "local-view" {
		if err := reuseOrDownload(claimed, localPath, descriptor.LocalAction); err != nil {
			return err
		}
	} else {
		if err := download(claimed.File.ContentURL, localPath); err != nil {
			return err
		}
		_ = os.Chmod(localPath, 0400)
	}

	if err := openFile(cfg, claimed.Mode, localPath); err != nil {
		return err
	}
	if claimed.Mode == "local-edit-exclusive" && claimed.Writeback != nil {
		return waitForStableChange(localPath, *claimed.Writeback, time.Unix(claimed.ExpiresAt, 0))
	}
	return nil
}

// localMirrorPath joins the workspace root with a sanitised server-relative
// path so a file at repo_id + path=/a/b/file.dwg lands at
// {root}/repo_id/a/b/file.dwg. claimed.Path already includes the file name
// (it is the full repo-relative path), so we must not append it again.
// Every path segment is cleaned to prevent traversal outside the root.
func localMirrorPath(root string, claimed session.Claimed) (string, error) {
	repoID := claimed.RepoID
	if repoID == "" {
		repoID = claimed.SessionID
	}
	rel := filepath.Clean(filepath.Join(string(filepath.Separator), claimed.Path))
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	full := filepath.Join(root, repoID, rel)
	// Defence in depth: the joined result must stay under root.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if absFull != absRoot && !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("session returned an unsafe path")
	}
	return full, nil
}

// reuseOrDownload reuses an existing local copy when its SHA1 matches the
// server file_id, otherwise resolves the conflict using LocalAction:
//   - "overwrite": re-download the server version (discard local edits)
//   - "keep_local": keep the local copy (edit then manually upload)
//   - "" (unset): default to keep_local for local-edit (preserve local work),
//     and overwrite for local-edit-exclusive (server is the source of truth).
func reuseOrDownload(claimed session.Claimed, localPath, action string) error {
	if _, err := os.Stat(localPath); err != nil {
		return download(claimed.File.ContentURL, localPath)
	}
	hash, err := Sha1File(localPath)
	if err != nil {
		return err
	}
	if claimed.FileID != "" && strings.EqualFold(hash, claimed.FileID) {
		return nil // already downloaded, reuse local copy
	}
	switch action {
	case "overwrite":
		return download(claimed.File.ContentURL, localPath)
	case "keep_local":
		return nil
	default:
		if claimed.Mode == "local-edit-exclusive" {
			return download(claimed.File.ContentURL, localPath)
		}
		return nil
	}
}

// Sha1File returns the lowercase hex SHA1 of a file's contents, matching
// Seafile's content-addressed file_id.
func Sha1File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha1.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
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

func waitForStableChange(path string, writeback session.Writeback, expires time.Time) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(filepath.Dir(path)); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Until(expires))
	defer deadline.Stop()
	heartbeat := time.NewTicker(5 * time.Minute)
	defer heartbeat.Stop()
	stabilityCheck := time.NewTicker(time.Second)
	defer stabilityCheck.Stop()
	var changedAt time.Time
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("file watcher stopped unexpectedly")
			}
			// Write 覆盖「直接写」；Rename 覆盖「旧文件被移走」；Create 覆盖
			// rename 原子替换保存时「新文件就位」的动作（Windows
			// RENAMED_NEW_NAME 被 fsnotify 映射为 Create）。缺了 Create 会
			// 漏掉 Word/WPS/Excel 这类「写临时文件再 rename 覆盖」的保存。
			if event.Name == path && event.Op&(fsnotify.Write|fsnotify.Rename|fsnotify.Create) != 0 {
				changedAt = time.Now()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("file watcher stopped unexpectedly")
			}
			return fmt.Errorf("watch local file: %w", err)
		case <-stabilityCheck.C:
			if !changedAt.IsZero() && time.Since(changedAt) >= 3*time.Second {
				return upload(path, writeback)
			}
		case <-heartbeat.C:
			if err := renew(writeback); err != nil {
				return fmt.Errorf("editing lease could not be renewed: %w", err)
			}
		case <-deadline.C:
			return fmt.Errorf("editing session expired before a stable file change was saved")
		}
	}
}

func renew(writeback session.Writeback) error {
	request, err := http.NewRequest(http.MethodPatch, writeback.HeartbeatURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+writeback.Capability)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat was rejected (%d)", response.StatusCode)
	}
	return nil
}

func upload(path string, writeback session.Writeback) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		defer writer.Close()
		part, err := form.CreateFormFile("file", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if closeErr := form.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = writer.CloseWithError(err)
		}
		writeDone <- err
	}()
	defer reader.Close()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, writeback.ContentURL, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+writeback.Capability)
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := <-writeDone; err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("file write-back failed (%d)", response.StatusCode)
	}
	return nil
}
