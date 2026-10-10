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
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/home"
	"github.com/eaopen/cloudfile-local-agent/internal/launch"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/runner"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/update"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const version = "1.0.1"

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
	updateExtension := flag.Bool("update-extension", false, "download and install the latest extension package")
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
			logUpdate("agent update failed: %v", err)
			fail(err)
		}
		return
	}
	if *updateExtension {
		if err := runUpdateExtension(); err != nil {
			logUpdate("extension update failed: %v", err)
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
	fmt.Fprintln(os.Stderr, "Upgrade with --check-update / --update / --update-extension.")
}

func handleNativeMessage(request nativehost.Request) nativehost.Response {
	switch request.Type {
	case "status":
		response := nativehost.Response{
			OK:           true,
			Version:      version,
			Applications: appfinder.Names(),
		}
		// 回传自身可执行文件路径，供扩展 popup 拼出「可直接复制运行」的升级命令。
		if executable, err := os.Executable(); err == nil {
			response.ExecutablePath = executable
		}
		if agentConfig, err := config.Load(); err == nil {
			if root, err := workspace.Resolve(agentConfig); err == nil {
				response.WorkspaceRoot = root
				response.CanOpenWorkspace = true
			}
			describeAgentUpdate(agentConfig, &response)
			describeExtensionUpdate(agentConfig, &response)
		}
		return response
	case "check_update":
		// 轻量更新探测：只回报版本与门禁状态，不附加应用清单/工作区等全量字段。
		// 扩展后台每 6 小时轮询它，用来点亮角标与弹系统通知。
		// 未配置更新源、Agent 未注册或离线时都返回 ok，只是没有新版本；
		// 这不是错误，扩展侧无需区分。
		if !request.Valid() {
			return nativehost.Response{Error: "invalid check_update request"}
		}
		response := nativehost.Response{OK: true, Version: version}
		if agentConfig, err := config.Load(); err == nil {
			describeAgentUpdate(agentConfig, &response)
			describeExtensionUpdate(agentConfig, &response)
		}
		return response
	case "update":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid update request"}
		}
		// 以子进程方式运行自身的 --update：它只下载新版本并改写 native host
		// manifest 的 path，不覆盖正在运行的二进制，所以立即返回、由子进程在后台完成。
		return spawnSelf("--update")
	case "update_extension":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid update_extension request"}
		}
		// 同理：下载并解压扩展包，落盘后由扩展自己 chrome.runtime.reload() 生效。
		return spawnSelf("--update-extension")
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
		// 镜像不再复刻库内目录树：一个资料库的本地副本全部平铺在
		// {root}/{view|edit}/{repo_id} 下，所以「打开本地目录」一律打开该资料库的
		// 平铺目录 —— 库内的子目录在本地并不存在，路径参数不再是定位依据。
		mode := request.MirrorMode()
		target := root
		if request.RepoID != "" {
			target, err = workspace.Dir(root, mode, request.RepoID)
			if err != nil {
				return nativehost.Response{Error: err.Error()}
			}
			if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
				// 该用途下还没有本地副本时，退到另一用途的同一资料库目录，
				// 避免把用户带到一个空目录。
				if alternate, alternateErr := workspace.Dir(root, alternateMode(mode), request.RepoID); alternateErr == nil {
					if _, alternateStat := os.Stat(alternate); alternateStat == nil {
						target = alternate
						mode = alternateMode(mode)
					}
				}
			}
			if err := os.MkdirAll(target, 0700); err != nil {
				return nativehost.Response{Error: err.Error()}
			}
		}
		if err := openDirectory(target); err != nil {
			return nativehost.Response{Error: err.Error()}
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
		// 本地副本按库内路径索引：磁盘上的文件名来自下载记录（可能因为重名而带 (1)），
		// 所以「有没有本地副本」只能由记录回答 —— 一个碰巧同名、却属于另一条库内路径的
		// 文件不算。
		name, recorded := workspace.FileName(root, mode, request.RepoID, request.Path)
		if name == "" {
			return nativehost.Response{Error: "invalid query_local_file path"}
		}
		if !recorded {
			dir, dirErr := workspace.Dir(root, mode, request.RepoID)
			if dirErr != nil {
				return nativehost.Response{Error: dirErr.Error()}
			}
			return nativehost.Response{OK: true, Mode: mode, LocalExists: false, LocalPath: filepath.Join(dir, name)}
		}
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
	case "query_local_folder":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid query_local_folder request"}
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
		dir, err := workspace.Dir(root, mode, request.RepoID)
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		// 只回答「这个资料库在该用途下建过本地目录没有」。网页用它决定
		// 「打开本地目录」入口要不要出现：镜像现在是一库一平铺目录，
		// 目录存在就等于用户至少在这里做过一次本地编辑。
		exists := false
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			exists = true
		}
		return nativehost.Response{OK: true, Mode: mode, LocalExists: exists, LocalPath: dir}
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

// openDirectory opens a folder in the system file manager.
// The mirror is flat, so there is never a file to highlight: one local folder
// holds every file of a library, and that folder is what gets opened.
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
		printExtensionStatus(agentConfig)
		return nil
	}
	newBinary, err := update.Apply(agentConfig.UpdateSource, manifest)
	if err != nil {
		return err
	}
	logUpdate("agent updated %s -> %s (%s)", version, manifest.Version, newBinary)
	fmt.Printf("updated to %s (%s)\n", manifest.Version, newBinary)
	fmt.Println("restart Chrome (or reload the extension) to pick up the new agent.")
	return nil
}

// runUpdateExtension drives --update-extension: download the extension package,
// verify it and replace the unpacked extension on disk. The extension itself
// then calls chrome.runtime.reload() to load the new files.
func runUpdateExtension() error {
	agentConfig, err := config.Load()
	if err != nil {
		return err
	}
	if agentConfig.UpdateSource == "" {
		return fmt.Errorf("update source is not configured (set update_source in config.json)")
	}
	source, err := update.ExtensionSourceURL(agentConfig.UpdateSource)
	if err != nil {
		return err
	}
	manifest, err := update.FetchExtension(source)
	if err != nil {
		return err
	}
	dir, err := update.ExtensionDir()
	if err != nil {
		return err
	}
	installed := update.InstalledExtensionVersion(dir)
	if compareExtended(manifest.Version, installed) <= 0 {
		fmt.Printf("CloudFile extension %s is up to date (latest %s)\n", orUnknown(installed), manifest.Version)
		return nil
	}
	installedDir, err := update.ApplyExtension(source, manifest, dir)
	if err != nil {
		return err
	}
	logUpdate("extension updated %s -> %s (%s)", orUnknown(installed), manifest.Version, installedDir)
	fmt.Printf("extension updated to %s (%s)\n", manifest.Version, installedDir)
	fmt.Println("reload the extension in chrome://extensions to activate it.")
	return nil
}

// printExtensionStatus reports the extension's update situation for
// --check-update, so a field engineer can see both sides from one command.
func printExtensionStatus(agentConfig config.Config) {
	manifest, err := update.FetchExtensionCached(agentConfig.UpdateSource, update.ExtensionCacheTTL)
	if err != nil {
		fmt.Printf("extension: manifest unavailable (%v)\n", err)
		return
	}
	dir, err := update.ExtensionDir()
	if err != nil {
		return
	}
	installed := update.InstalledExtensionVersion(dir)
	if compareExtended(manifest.Version, installed) > 0 {
		fmt.Printf("extension update available: %s -> %s\n", orUnknown(installed), manifest.Version)
		return
	}
	fmt.Printf("CloudFile extension %s is up to date (latest %s)\n", orUnknown(installed), manifest.Version)
}

// describeAgentUpdate fills the agent-side update fields from the cached
// manifest. A fetch failure leaves them empty: an unreachable source must never
// advertise a version it cannot actually install. Nothing is ever refused
// because of a version comparison — an available update is only a prompt.
func describeAgentUpdate(agentConfig config.Config, response *nativehost.Response) {
	response.UpdateSourceSet = agentConfig.UpdateSource != ""
	if agentConfig.UpdateSource == "" {
		return
	}
	manifest, err := update.FetchCached(agentConfig.UpdateSource, update.CacheTTL)
	if err != nil {
		return
	}
	if comparison, err := update.Compare(manifest.Version, version); err == nil && comparison > 0 {
		response.UpdateAvailable = true
		response.LatestVersion = manifest.Version
		response.LatestNotes = manifest.Notes
	}
}

// describeExtensionUpdate fills the extension-side fields, including the
// version currently sitting on disk — the extension waits for that to match the
// announced version before reloading itself. The extension compares the
// announced version against its own, so the agent does not need to be told it.
func describeExtensionUpdate(agentConfig config.Config, response *nativehost.Response) {
	if dir, err := update.ExtensionDir(); err == nil {
		response.ExtensionInstalledVersion = update.InstalledExtensionVersion(dir)
	}
	if agentConfig.UpdateSource == "" {
		return
	}
	manifest, err := update.FetchExtensionCached(agentConfig.UpdateSource, update.ExtensionCacheTTL)
	if err != nil {
		return
	}
	response.ExtensionLatestVersion = manifest.Version
	response.ExtensionNotes = manifest.Notes
}

// spawnSelf runs this executable with args as a detached background process.
// Chrome tears the native-messaging host down as soon as it has read the
// response (and on Windows may kill its whole job object), so upgrade work has
// to happen in a process that outlives the host.
func spawnSelf(args ...string) nativehost.Response {
	executable, err := os.Executable()
	if err != nil {
		return nativehost.Response{Error: "cannot start local agent"}
	}
	if err := launch.Detached(executable, args...); err != nil {
		return nativehost.Response{Error: err.Error()}
	}
	return nativehost.Response{OK: true}
}

// compareExtended orders a candidate version against a possibly-unreadable
// reference: an absent or malformed reference counts as "older than anything",
// so a fresh install is treated as needing an update rather than as up to date.
func compareExtended(candidate, reference string) int {
	if reference == "" {
		return 1
	}
	comparison, err := update.Compare(candidate, reference)
	if err != nil {
		return 1
	}
	return comparison
}

func orUnknown(value string) string {
	if value == "" {
		return "(unknown)"
	}
	return value
}

// logUpdate appends one line to the agent's update log. A detached upgrade has
// no console, so this file is the only way to diagnose a failed upgrade.
func logUpdate(format string, args ...any) {
	path := filepath.Join(home.Dir(), "update.log")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
