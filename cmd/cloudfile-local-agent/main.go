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
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/home"
	"github.com/eaopen/cloudfile-local-agent/internal/launch"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/runner"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
	"github.com/eaopen/cloudfile-local-agent/internal/update"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

const version = "2.0.3"

func main() {
	inspectLocal := flag.Bool("inspect-local-task-stdin", false, "private same-user local task info for the original desktop controller")
	manageLocal := flag.Bool("manage-local-task-stdin", false, "macOS only: user-selected original local edit controller")
	indexSession := flag.Bool("index-local-session-stdin", false, "local task index only; no editing or authorization")
	optimisticUpload := flag.Bool("optimistic-upload-stdin", false, "explicit one-shot paired-device optimistic local file upload")
	editUpload := flag.Bool("edit-upload-stdin", false, "explicit manual upload/recovery from a local controller descriptor")
	checkoutInit := flag.Bool("init-checkout-stdin", false, "initialize a signed device Checkout with a durable journal")
	checkoutInspect := flag.Bool("inspect-checkout-stdin", false, "inspect one local pending intent without secret output")
	checkoutRenew := flag.Bool("renew-checkout-stdin", false, "renew one active device Checkout without upload")
	pairingAction := flag.String("pairing", "", "macOS local enrollment: start, sign, confirm, cancel")
	pairingInstance := flag.String("pairing-instance", "", "explicit HTTPS instance for enrollment")
	pairingAlias := flag.String("pairing-key", "", "local pending enrollment key alias")
	component := flag.Bool("component", false, "serve one bounded internal component JSON request")
	nativeHost := flag.Bool("native-host", false, "serve Chrome Native Messaging")
	showVersion := flag.Bool("version", false, "print Agent2 version")
	editingCapabilities := flag.Bool("editing-capabilities", false, "report supported local editing stages, without credentials")
	runSession := flag.String("run-session", "", "run one CloudFile session file")
	openURIStdin := flag.Bool("open-uri-stdin", false, "open one paired URI from local pipe")
	runSessionStdin := flag.Bool("run-session-stdin", false, "run one CloudFile session descriptor from stdin")
	allowOrigin := flag.String("allow-origin", "", "trust one CloudFile server origin")
	setUpdateKey := flag.String("set-update-key", "", "pin a local P-256 release public PEM file")
	setUpdateSource := flag.String("set-update-source", "", "record the static update.json URL")
	showConfig := flag.Bool("show-config", false, "print local configuration")
	validateConfig := flag.Bool("validate-config", false, "validate local configuration")
	checkUpdate := flag.Bool("check-update", false, "check for and report an available update")
	applyUpdate := flag.Bool("update", false, "check, download and install the latest update")
	updateExtension := flag.Bool("update-extension", false, "download and install the latest extension package")
	flag.Parse()
	if *inspectLocal {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("private task inspection accepts stdin only"))
		}
		if err := runInspectLocalTask(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *manageLocal {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("local user action accepts stdin only"))
		}
		if err := runManageLocalTask(os.Stdin); err != nil {
			fail(err)
		}
		return
	}
	if *indexSession {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("local task indexing accepts only stdin"))
		}
		if err := indexLocalSession(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *checkoutRenew {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("Checkout renewal accepts only local stdin"))
		}
		if err := runCheckoutRenew(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *checkoutInspect {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("checkout inspection accepts only local stdin"))
		}
		if err := runCheckoutInspect(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *checkoutInit {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("checkout initialization accepts only local stdin"))
		}
		if err := runCheckoutInit(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *optimisticUpload {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("optimistic upload requires a single local stdin request"))
		}
		if err := runOptimisticUpload(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *editUpload {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("edit upload accepts only local stdin"))
		}
		if err := runEditUpload(os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *pairingAction != "" {
		if flag.NFlag() != 2 || flag.NArg() != 0 {
			fail(fmt.Errorf("pairing requires exactly action and instance or key"))
		}
		if err := runPairing(*pairingAction, *pairingInstance, *pairingAlias); err != nil {
			fail(err)
		}
		return
	}

	if *component {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			os.Exit(1)
		}
		if err := serveComponent(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *editingCapabilities {
		data, err := json.Marshal(map[string]any{
			"family": "agent2", "core_version": version, "platform": runtime.GOOS,
			"manual_submit_core": true, "device_signed_prepare": true,
			"native_device_publish": false, "automatic_upload": false,
		})
		if err != nil {
			fail(err)
		}
		fmt.Println(string(data))
		return
	}
	if *allowOrigin != "" {
		if _, err := config.AddOrigin(*allowOrigin); err != nil {
			fail(err)
		}
		return
	}
	if *setUpdateKey != "" {
		data, err := os.ReadFile(*setUpdateKey)
		if err != nil {
			fail(err)
		}
		if _, err := update.PublicKey(string(data)); err != nil {
			fail(err)
		}
		cfg, err := config.Load()
		if err != nil {
			fail(err)
		}
		cfg.UpdatePublicKey = string(data)
		if err := config.Save(cfg); err != nil {
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
	if *openURIStdin {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			fail(fmt.Errorf("fixed paired URI pipe required"))
		}
		if err := runOpenURIFromStdin(); err != nil {
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
		if *applyUpdate {
			recordUpdateResult("agent", "running")
		}
		if err := runUpdate(*applyUpdate); err != nil {
			if *applyUpdate {
				recordUpdateResult("agent", "failed")
			}
			logUpdate("agent update failed: %v", err)
			fail(err)
		}
		if *applyUpdate {
			recordUpdateResult("agent", "completed")
		}
		return
	}
	if *updateExtension {
		recordUpdateResult("extension", "running")
		if err := runUpdateExtension(); err != nil {
			recordUpdateResult("extension", "failed")
			logUpdate("extension update failed: %v", err)
			fail(err)
		}
		recordUpdateResult("extension", "completed")
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

// Windows bundles Go as an internal child of the single .NET Host.
// Do not advertise network session actions before an explicit trusted origin
// exists. The controller delegates only advertised operations.
func advertisedCapabilities(trustedOrigins []string) []string {
	operations := []string{"check_update"}
	// The macOS Agent2 Host advertises a gated URI receiver. It never grants
	// access without a confirmed local Keychain pairing and a live one-use
	// server claim, verified only by the receiver worker.
	if runtime.GOOS == "darwin" {
		operations = append(operations, "open_uri")
	}
	if len(trustedOrigins) != 0 {
		operations = append(operations, "open_session", "open_session_file",
			"query_local_file", "query_local_folder", "open_workspace")
	}
	return operations
}

func handleNativeMessage(request nativehost.Request) nativehost.Response {
	switch request.Type {
	case "status":
		response := nativehost.Response{
			OK:           true,
			Version:      version,
			AgentFamily:  "agent2",
			Platform:     runtime.GOOS,
			Capabilities: advertisedCapabilities(nil),
			Applications: appfinder.Names(),
		}
		// 回传自身可执行文件路径，供扩展 popup 拼出「可直接复制运行」的升级命令。
		if executable, err := os.Executable(); err == nil {
			response.ExecutablePath = executable
		}
		if agentConfig, err := config.Load(); err == nil {
			if len(agentConfig.AllowedOrigins) > 0 {
				// A newly installed internal Windows Go component MUST NOT take
				// over an existing session until its trusted CloudFile origin is
				// configured. Chrome routes by capabilities, not installation.
				response.Capabilities = advertisedCapabilities(agentConfig.AllowedOrigins)
				if root, err := workspace.Resolve(agentConfig); err == nil {
					response.WorkspaceRoot = root
					response.CanOpenWorkspace = true
				}
			}
			describeAgentUpdate(agentConfig, &response)
			describeExtensionUpdate(agentConfig, &response)
			// macOS Go is the registered Chrome Host; Windows retains .NET.
			if runtime.GOOS == "darwin" {
				if root, err := agentConfig.Root(); err == nil {
					if store, err := taskcenter.New(root); err == nil {
						if _, err := store.List(); err == nil {
							response.Capabilities = append(response.Capabilities, "task_summary")
							if store.ResidentOnline(time.Now()) {
								response.Capabilities = append(response.Capabilities, "focus_task")
							}
						}
					}
				}
			}
		}
		return response
	case "focus_task":
		if !request.Valid() || runtime.GOOS != "darwin" {
			return nativehost.Response{Error: "invalid local focus request"}
		}
		cfg, err := config.Load()
		if err != nil {
			return nativehost.Response{Error: "local task configuration unavailable"}
		}
		root, err := cfg.Root()
		if err != nil {
			return nativehost.Response{Error: "local task workspace unavailable"}
		}
		store, err := taskcenter.New(root)
		if err != nil || store.RequestFocus(request.TaskID, time.Now()) != nil {
			return nativehost.Response{Error: "task is not active in the local TaskCenter"}
		}
		return nativehost.Response{OK: true}
	case "task_summary":
		if !request.Valid() || runtime.GOOS != "darwin" {
			return nativehost.Response{Error: "local task summary unsupported"}
		}
		cfg, err := config.Load()
		if err != nil {
			return nativehost.Response{Error: "local task configuration unavailable"}
		}
		root, err := cfg.Root()
		if err != nil {
			return nativehost.Response{Error: "local task workspace unavailable"}
		}
		store, err := taskcenter.New(root)
		if err != nil {
			return nativehost.Response{Error: "local task index unavailable"}
		}
		tasks, err := store.List()
		if err != nil {
			return nativehost.Response{Error: "local task recovery requires attention"}
		}
		result := make([]nativehost.TaskSummary, 0, len(tasks))
		for _, t := range tasks {
			if len(result) >= 50 {
				break
			}
			result = append(result, nativehost.TaskSummary{
				ID: t.ID, FileName: t.FileName, Mode: t.Mode,
				Changed: t.LocalChanged, NeedsReview: t.NeedsReview,
			})
		}
		return nativehost.Response{OK: true, Tasks: result}
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
		recordUpdateResult("agent", "running")
		result := spawnSelf("--update")
		if !result.OK {
			recordUpdateResult("agent", "failed")
		}
		return result
	case "update_extension":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid update_extension request"}
		}
		// 同理：下载并解压扩展包，落盘后由扩展自己 chrome.runtime.reload() 生效。
		recordUpdateResult("extension", "running")
		result := spawnSelf("--update-extension")
		if !result.OK {
			recordUpdateResult("extension", "failed")
		}
		return result
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
	case "open_uri":
		if runtime.GOOS != "darwin" || !request.Valid() {
			return nativehost.Response{Error: "macOS paired URI required"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "installed local Agent unavailable"}
		}
		if err = launch.WithInput(executable, []byte(request.URI), "--open-uri-stdin"); err != nil {
			return nativehost.Response{Error: "local session controller unavailable"}
		}
		return nativehost.Response{OK: true}
	case "open_session_file":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid session file path"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "cannot start local agent"}
		}
		if err := launch.Detached(executable, "--run-session", request.Path); err != nil {
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
		if err := launch.WithInput(executable, data, "--run-session-stdin"); err != nil {
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
	manifest, err := fetchAgentRelease(agentConfig, 30*time.Second)
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
	manifest, err := fetchExtensionRelease(agentConfig)
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
	manifest, err := fetchExtensionRelease(agentConfig)
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
	describeUpdateResult(response)
	response.UpdateSourceSet = agentConfig.UpdateSource != ""
	if agentConfig.UpdateSource == "" {
		return
	}
	manifest, err := fetchAgentRelease(agentConfig, 2*time.Second)
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
	manifest, err := fetchExtensionRelease(agentConfig)
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

// macOS releases require a locally pinned signing key. Windows uses its .NET
// bundle updater; legacy Unix compatibility is kept separate.
func fetchAgentRelease(cfg config.Config, timeout time.Duration) (*update.Manifest, error) {
	if runtime.GOOS == "darwin" {
		return update.FetchSigned(cfg.UpdateSource, cfg.UpdatePublicKey, timeout)
	}
	return update.FetchCached(cfg.UpdateSource, update.CacheTTL)
}

func fetchExtensionRelease(cfg config.Config) (*update.ExtensionManifest, error) {
	if runtime.GOOS == "darwin" {
		release, err := fetchAgentRelease(cfg, 2*time.Second)
		if err != nil {
			return nil, err
		}
		if release.Extension == nil {
			return nil, fmt.Errorf("signed extension artifact unavailable")
		}
		return release.Extension, nil
	}
	return update.FetchExtensionCached(cfg.UpdateSource, update.ExtensionCacheTTL)
}
