package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/runner"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/update"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const version = "0.5.5"

func main() {
	nativeHost := flag.Bool("native-host", false, "serve Chrome Native Messaging")
	runSession := flag.String("run-session", "", "run one CloudFile session file")
	runSessionStdin := flag.Bool("run-session-stdin", false, "run one CloudFile session descriptor from stdin")
	allowOrigin := flag.String("allow-origin", "", "trust one CloudFile server origin")
	setUpdateSource := flag.String("set-update-source", "", "record the static update.json URL")
	showConfig := flag.Bool("show-config", false, "print local configuration")
	validateConfig := flag.Bool("validate-config", false, "validate local configuration")
	checkUpdate := flag.Bool("check-update", false, "check for and report an available update")
	applyUpdate := flag.Bool("update", false, "check, download and install the latest update")
	flag.Parse()

	if *allowOrigin != "" {
		if _, err := config.AddOrigin(*allowOrigin); err != nil {
			fail(err)
		}
		return
	}
	if *setUpdateSource != "" {
		if _, err := config.SetUpdateSource(*setUpdateSource); err != nil {
			fail(err)
		}
		return
	}
	if *showConfig || *validateConfig {
		agentConfig, err := config.Load()
		if err != nil {
			fail(err)
		}
		if *showConfig {
			data, err := json.MarshalIndent(agentConfig, "", "  ")
			if err != nil {
				fail(err)
			}
			fmt.Println(string(data))
		}
		return
	}
	if *runSession != "" {
		if err := runner.Run(*runSession); err != nil {
			fail(err)
		}
		return
	}
	if *runSessionStdin {
		if err := runSessionFromStdin(); err != nil {
			fail(err)
		}
		return
	}
	if *checkUpdate || *applyUpdate {
		if err := runUpdate(*applyUpdate); err != nil {
			fail(err)
		}
		return
	}
	if *nativeHost || flag.NFlag() == 0 {
		if err := nativehost.Serve(os.Stdin, os.Stdout, handleNativeMessage); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "CloudFile Local Agent %s\n", version)
	fmt.Fprintln(os.Stderr, "Use --allow-origin https://cloudfile.example before opening sessions.")
}

func handleNativeMessage(request nativehost.Request) nativehost.Response {
	switch request.Type {
	case "status":
		response := nativehost.Response{
			OK:           true,
			Version:      version,
			Applications: appfinder.Names(),
		}
		if agentConfig, err := config.Load(); err == nil {
			if root, err := workspace.Resolve(agentConfig); err == nil {
				response.WorkspaceRoot = root
				response.CanOpenWorkspace = true
			}
			response.UpdateSourceSet = agentConfig.UpdateSource != ""
			if agentConfig.UpdateSource != "" {
				if manifest, err := update.Fetch(agentConfig.UpdateSource); err == nil {
					if comparison, err := update.Compare(manifest.Version, version); err == nil && comparison > 0 {
						response.UpdateAvailable = true
						response.LatestVersion = manifest.Version
					}
				}
			}
		}
		return response
	case "open_workspace":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid open_workspace request"}
		}
		agentConfig, err := config.Load()
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		root, err := workspace.Resolve(agentConfig)
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		// 打开某个用途下的库/目录镜像；Path 为空时到库级目录，否则到 Path 指向的文件/目录。
		// repo_id 为空时打开镜像根，两个用途子树都在下面。
		mode := request.MirrorMode()
		target := root
		if request.RepoID != "" {
			target, err = workspace.Path(root, mode, request.RepoID, request.Path)
			if err != nil {
				return nativehost.Response{Error: err.Error()}
			}
			if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
				// 该用途下还没有本地副本时，退到另一用途的同一位置，
				// 避免把用户带到一个空目录。
				if alternate, alternateErr := workspace.Path(root, alternateMode(mode), request.RepoID, request.Path); alternateErr == nil {
					if _, alternateStat := os.Stat(alternate); alternateStat == nil {
						target = alternate
					}
				}
			}
		}
		// 目标是已存在的文件 → 用 /select 在文件管理器中高亮；已存在的目录 → 直接打开；
		// 均不存在 → 回退打开父目录（MkdirAll 保证存在）。
		if info, statErr := os.Stat(target); statErr == nil {
			if info.IsDir() {
				if err := openDirectory(target); err != nil {
					return nativehost.Response{Error: err.Error()}
				}
			} else {
				if err := openDirectorySelect(target); err != nil {
					return nativehost.Response{Error: err.Error()}
				}
			}
		} else {
			parent := filepath.Dir(target)
			if err := os.MkdirAll(parent, 0700); err != nil {
				return nativehost.Response{Error: err.Error()}
			}
			if err := openDirectory(parent); err != nil {
				return nativehost.Response{Error: err.Error()}
			}
		}
		return nativehost.Response{OK: true, Mode: mode, WorkspaceRoot: root, LocalPath: target}
	case "query_local_file":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid query_local_file request"}
		}
		agentConfig, err := config.Load()
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		root, err := workspace.Resolve(agentConfig)
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		mode := request.MirrorMode()
		// request.Path 是含文件名的库内完整路径（如 /dir/file.dwg）。
		localPath, err := workspace.Path(root, mode, request.RepoID, request.Path)
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			return nativehost.Response{OK: true, Mode: mode, LocalExists: false, LocalPath: localPath}
		}
		response := nativehost.Response{
			OK:          true,
			Mode:        mode,
			LocalExists: true,
			LocalPath:   localPath,
			LocalSize:   info.Size(),
			LocalMTime:  info.ModTime().Unix(),
		}
		// 指纹只在下过这份副本时才有。没有指纹就无法判断本地是否被改过，
		// 也就不必为一个大文件白读一遍内容，交给网页按「无法确认」处理。
		if record, found := workspace.ReadRecord(root, mode, request.RepoID, request.Path); found {
			response.BaselineExists = true
			response.BaselineFileID = record.FileID
			response.BaselineDigest = record.Digest
			response.BaselineAt = record.StoredAt
			if record.Digest != "" {
				sha1Sum, digest, hashErr := workspace.Hashes(localPath)
				if hashErr != nil {
					return nativehost.Response{Error: hashErr.Error()}
				}
				response.LocalHash = sha1Sum
				response.LocalDigest = digest
				response.LocalChanged = record.Digest != digest
			}
		}
		return response
	case "open_session_file":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid session file path"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "cannot start local agent"}
		}
		command := exec.Command(executable, "--run-session", request.Path)
		command.Stdout = nil
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		return nativehost.Response{OK: true}
	case "open_session":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid session descriptor"}
		}
		descriptor := session.Descriptor{
			Protocol:    request.Protocol,
			Server:      request.Server,
			Ticket:      request.Ticket,
			ExpiresAt:   request.ExpiresAt,
			LocalAction: request.LocalAction,
		}
		data, err := json.Marshal(descriptor)
		if err != nil {
			return nativehost.Response{Error: "cannot encode session descriptor"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "cannot start local agent"}
		}
		command := exec.Command(executable, "--run-session-stdin")
		command.Stdin = strings.NewReader(string(data))
		command.Stdout = nil
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		return nativehost.Response{OK: true}
	default:
		return nativehost.Response{Error: "unsupported native message"}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "CloudFile Local Agent:", err)
	os.Exit(1)
}

// alternateMode is the other mirror subtree. It is used to fall back when the
// requested subtree holds no local copy, so the user is never dropped into an
// empty folder while their file sits in the other one.
func alternateMode(mode string) string {
	if mode == workspace.ModeView {
		return workspace.ModeEdit
	}
	return workspace.ModeView
}

func openDirectory(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("explorer.exe", path)
	case "darwin":
		command = exec.Command("open", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}

// openDirectorySelect 打开文件所在目录并在文件管理器中高亮该文件。
// Windows: explorer /select,<path>；macOS: open -R；其他: 回退打开父目录。
// 用 exec.Command 单参数传参（不经 shell），路径含空格/中文均安全。
func openDirectorySelect(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// /select, 与路径必须连成一个参数；否则 explorer 会忽略。
		command = exec.Command("explorer.exe", "/select,"+path)
	case "darwin":
		command = exec.Command("open", "-R", path)
	default:
		command = exec.Command("xdg-open", filepath.Dir(path))
	}
	return command.Start()
}

func runSessionFromStdin() error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	descriptor, err := session.Parse(data)
	if err != nil {
		return err
	}
	return runner.RunDescriptor(descriptor)
}

// runUpdate drives the --check-update / --update commands. With apply=false it
// only reports whether an update is available; with apply=true it downloads,
// verifies and installs it.
func runUpdate(apply bool) error {
	agentConfig, err := config.Load()
	if err != nil {
		return err
	}
	if agentConfig.UpdateSource == "" {
		return fmt.Errorf("update source is not configured (set update_source in config.json)")
	}
	manifest, err := update.Fetch(agentConfig.UpdateSource)
	if err != nil {
		return err
	}
	comparison, err := update.Compare(manifest.Version, version)
	if err != nil {
		return fmt.Errorf("update manifest has invalid version %q: %w", manifest.Version, err)
	}
	if comparison <= 0 {
		fmt.Printf("CloudFile Local Agent %s is up to date (latest %s)\n", version, manifest.Version)
		return nil
	}
	if !apply {
		fmt.Printf("update available: %s -> %s\n", version, manifest.Version)
		if manifest.Notes != "" {
			fmt.Println(manifest.Notes)
		}
		return nil
	}
	newBinary, err := update.Apply(agentConfig.UpdateSource, manifest)
	if err != nil {
		return err
	}
	fmt.Printf("updated to %s (%s)\n", manifest.Version, newBinary)
	fmt.Println("restart Chrome (or reload the extension) to pick up the new agent.")
	return nil
}
