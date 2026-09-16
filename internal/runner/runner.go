package runner

import (
	"context"
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
	workspace := filepath.Join(root, claimed.SessionID, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return err
	}
	name := filepath.Base(claimed.File.Name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return fmt.Errorf("session returned an invalid file name")
	}
	localPath := filepath.Join(workspace, name)
	if err := download(claimed.File.ContentURL, localPath); err != nil {
		return err
	}
	if claimed.Mode == "local-view" {
		_ = os.Chmod(localPath, 0400)
	}
	if err := openFile(cfg, claimed.Mode, localPath); err != nil {
		return err
	}
	if claimed.Mode == "local-edit" && claimed.Writeback != nil {
		return waitForStableChange(localPath, *claimed.Writeback, time.Unix(claimed.ExpiresAt, 0))
	}
	return nil
}

func download(rawURL, target string) error {
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Get(rawURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("file download failed (%d)", response.StatusCode)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
