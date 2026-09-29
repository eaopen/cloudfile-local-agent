package runner

import (
	"crypto/sha1"
	"encoding/hex"
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

// reuseOrDownload reuses an existing local copy according to the caller's
// explicit action, or downloads when no local copy exists.
//   - "overwrite": re-download the server version (discard local edits)
//   - "keep_local": keep the local copy (edit then manually upload)
//   - "" (unset): default to keep_local for local-edit (preserve local work).
//
// 注意：不能拿本地文件内容 SHA1 与 claimed.FileID 比对来判断「是否已下载」。
// FileID 是 Seafile obj_id（文件元数据 JSON 的 SHA1），并非内容 SHA1，两者
// 语义不同、永远不等。真正的「网盘是否被更新」判定由前端用「下载时记录的
// fileId vs 当前 fileId」完成，这里只负责按前端决议的 action 落地。
func reuseOrDownload(claimed session.Claimed, localPath, action string) error {
	if _, err := os.Stat(localPath); err != nil {
		return download(claimed.File.ContentURL, localPath)
	}
	switch action {
	case "overwrite":
		return download(claimed.File.ContentURL, localPath)
	case "keep_local":
		return nil
	default:
		return nil
	}
}

// Sha1File returns the lowercase hex SHA1 of a file's contents.
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
