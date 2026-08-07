package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/fsnotify/fsnotify"
)

func Run(path string) error {
	descriptor, err := session.Read(path)
	if err != nil {
		return err
	}
	config, err := config.Load()
	if err != nil {
		return err
	}
	if !config.Allows(descriptor.Server) {
		return fmt.Errorf("the CloudFile origin is not trusted by this agent")
	}
	claimed, err := session.Claim(descriptor)
	if err != nil {
		return err
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	workspace := filepath.Join(root, claimed.SessionID, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return err
	}
	localPath := filepath.Join(workspace, filepath.Base(claimed.File.Name))
	if err := download(claimed.File.ContentURL, localPath); err != nil {
		return err
	}
	if claimed.Mode == "local-view" {
		_ = os.Chmod(localPath, 0400)
	}
	if err := openDefault(localPath); err != nil {
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
	_, err = io.Copy(file, io.LimitReader(response.Body, 4*1024*1024*1024))
	return err
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
	var changedAt time.Time
	for {
		select {
		case event := <-watcher.Events:
			if event.Name == path && event.Op&(fsnotify.Write|fsnotify.Rename) != 0 {
				changedAt = time.Now()
			}
		case <-time.After(time.Second):
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
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, writeback.ContentURL, &body)
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
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("file write-back failed (%d)", response.StatusCode)
	}
	return nil
}
