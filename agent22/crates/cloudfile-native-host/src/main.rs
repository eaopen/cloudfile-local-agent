//! Chrome native messaging host and the agent's own installer entry point.
//!
//! With no arguments this is a stdio host: Chrome writes length-prefixed JSON,
//! and every request is either answered locally or forwarded to the desktop
//! process over the file mailbox. With arguments it is a plain CLI, which is
//! how registration, login items, upgrades and reverts are performed without
//! a scripting runtime on the target machine.
use cloudfile_core::{
    apps, autostart, device, guard, hostreg, ipc, native, paired_session, session, storage, update,
    wire,
};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};

const USAGE: &str = "\
CloudFile Agent 2.2

Usage: cloudfile-native-host [command]

  --version                      print the build version
  --component                    print the local component contract as JSON
  --status                       print agent, registration and exit state as JSON
  --register-host                register the Chrome host for the current user
  --unregister-host              remove the host this agent registered
  --rollback-host                restore the previous host registration
  --host-state                   print the current Chrome host registration
  --enable-autostart             start the agent at login
  --disable-autostart            stop starting the agent at login
  --check-update                 report the available signed release, if any
  --update                       download, verify, stage and activate an update
  --update-from <manifest>       run an update from a local signed manifest
  --update-extension             install the newest browser extension and let it reload
  --rollback                     point the host back at the previous version
  --prune                        keep the active version plus one revert point

Registration options:
  --extension-id <id>            Chrome extension ID to allow (32 chars, a-p)
  --host-name <name>             host name (defaults to the development host)
  --takeover                     back up and replace a host another program owns
";

const DEV_USAGE: &str = "cloudfile-native-host --register-host --extension-id <id>";

fn flag(args: &[String], name: &str) -> Option<String> {
    let index = args.iter().position(|value| value == name)?;
    args.get(index + 1).cloned()
}

fn root() -> anyhow::Result<PathBuf> {
    storage::root()
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.is_empty() {
        if run_stdio().is_err() {
            eprintln!("CloudFile native messaging failed");
            std::process::exit(1);
        }
        return;
    }
    if let Err(error) = run_command(&args) {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

fn run_command(args: &[String]) -> anyhow::Result<()> {
    let root = root()?;
    let host = flag(args, "--host-name").unwrap_or_else(|| hostreg::DEV_HOST.to_string());
    let command = args[0].as_str();
    match command {
        "--version" => println!("{}", cloudfile_core::VERSION),
        "--component" => {
            let value = json!({
                "version": cloudfile_core::VERSION,
                "ipc_version": update::IPC_VERSION,
                "journal_version": update::JOURNAL_VERSION,
                "agent_family": "agent2",
                "platform": update::platform_id(std::env::consts::OS, std::env::consts::ARCH),
            });
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        "--status" => {
            let binary = host_binary()?;
            let dir = hostreg::manifest_dir()?;
            let registration = hostreg::state(&dir, &host, Some(&binary))?;
            let value = json!({
                "version": cloudfile_core::VERSION,
                "install_root": update::install_root(&binary).to_string_lossy(),
                "host": host,
                "registered": registration.binary.is_some(),
                "foreign": registration.foreign,
                "manifest_path": registration.manifest_path.to_string_lossy(),
                "binary": registration.binary.as_ref().map(|p| p.to_string_lossy().to_string()),
                "extension_id": registration.extension_id,
                "active_version": update::active_version(&dir, &host)?,
                "installed_versions": update::installed_versions(&update::install_root(&binary))?,
                "autostart": autostart::enabled(),
                "autostart_mechanism": autostart::mechanism().map(|item| item.id()),
                "applications": apps::names(),
                "runtime": {
                    // The desktop shell needs WebView2 on Windows; on macOS the
                    // system WebKit is always present, so `null` there means the
                    // check does not apply, not that it failed.
                    "webview2": apps::webview2_version(),
                },
                "exit": guard::assess(&root),
            });
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        "--register-host" => {
            let extension_id = flag(args, "--extension-id")
                .ok_or_else(|| anyhow::anyhow!("--extension-id is required\n\n{DEV_USAGE}"))?;
            let takeover = args.iter().any(|value| value == "--takeover");
            let binary = host_binary()?;
            let dir = hostreg::manifest_dir()?;
            // A host owned by another program is never replaced on the strength
            // of a failed attempt alone: the caller asks for that explicitly so
            // a script cannot escalate by accident.
            let outcome = hostreg::install(&dir, &host, &extension_id, &binary, takeover).map_err(
                |error| {
                    anyhow::anyhow!(
                        "{error}\n\n如果确认这属于本机 Agent，请加 --takeover 重新注册；原注册会先备份并可用 --rollback-host 还原"
                    )
                },
            )?;
            println!("{}", serde_json::to_string_pretty(&outcome)?);
            // Registration succeeds without it — the native host works either
            // way — but the desktop shell will not start, so say so now rather
            // than let it fail opaquely later.
            if cfg!(windows) && apps::webview2_version().is_none() {
                eprintln!(
                    "警告：未检测到 WebView2 运行时，任务中心窗口可能无法启动。\n\
                     请安装 Microsoft Edge WebView2 Runtime（当前 Windows 通常已内置）。"
                );
            }
        }
        "--unregister-host" => {
            let dir = hostreg::manifest_dir()?;
            let removed = hostreg::unregister(&dir, &host)?;
            println!("{}", removed.to_string_lossy());
        }
        "--rollback-host" => {
            let dir = hostreg::manifest_dir()?;
            println!("{}", hostreg::rollback(&dir, &host)?.to_string_lossy());
        }
        "--host-state" => {
            let dir = hostreg::manifest_dir()?;
            let binary = host_binary()?;
            let registration = hostreg::state(&dir, &host, Some(&binary))?;
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "host": registration.host,
                    "manifest_path": registration.manifest_path.to_string_lossy(),
                    "registered": registration.binary.is_some(),
                    "binary": registration.binary.as_ref().map(|p| p.to_string_lossy().to_string()),
                    "extension_id": registration.extension_id,
                    "foreign": registration.foreign,
                }))?
            );
        }
        "--enable-autostart" => {
            let binary = desktop_binary()?;
            // 说清落在哪条途径上：企业机器上 Run 键常被锁，实际生效的是
            // 「启动」文件夹 —— 不报出来就没法解释「为什么任务管理器里看不到」。
            let item = autostart::install(&root, &binary)?;
            println!("{}", binary.to_string_lossy());
            println!("{}", item.id());
        }
        "--disable-autostart" => {
            autostart::remove(&root)?;
            println!("disabled");
        }
        "--check-update" => {
            println!(
                "{}",
                serde_json::to_string_pretty(&native::check_update(&root))?
            );
        }
        "--update" => run_update(&root, &host, None)?,
        "--update-extension" => {
            native::record_update_result(&root, "extension", "running");
            match native::update_extension(&root) {
                Ok(message) => {
                    native::record_update_result(&root, "extension", "completed");
                    println!("{message}");
                }
                Err(error) => {
                    native::record_update_result(&root, "extension", "failed");
                    return Err(error);
                }
            }
        }
        "--update-from" => {
            let path = flag(args, "--update-from")
                .ok_or_else(|| anyhow::anyhow!("--update-from requires a manifest path"))?;
            run_update(&root, &host, Some(PathBuf::from(path)))?;
        }
        "--rollback" => {
            let dir = hostreg::manifest_dir()?;
            println!("{}", update::rollback(&dir, &host)?.to_string_lossy());
        }
        "--prune" => {
            let binary = host_binary()?;
            let install = update::install_root(&binary);
            let dir = hostreg::manifest_dir()?;
            let removed = update::prune(&install, &host, &dir, 2)?;
            println!("{}", serde_json::to_string_pretty(&removed)?);
        }
        "--help" | "-h" => print!("{USAGE}"),
        other => anyhow::bail!("unknown command {other}\n\n{USAGE}"),
    }
    Ok(())
}

fn host_binary() -> anyhow::Result<PathBuf> {
    let binary = std::env::current_exe()?;
    Ok(binary.canonicalize().unwrap_or(binary))
}

/// The desktop executable is installed next to the native host; the mailbox
/// protocol depends on that, and so does the login item.
fn desktop_binary() -> anyhow::Result<PathBuf> {
    let host = host_binary()?;
    let name = if cfg!(windows) {
        "cloudfile-desktop.exe"
    } else {
        "cloudfile-desktop"
    };
    let directory = host
        .parent()
        .ok_or_else(|| anyhow::anyhow!("native host has no install directory"))?;
    let candidate = directory.join(name);
    if !candidate.is_file() {
        anyhow::bail!("cloudfile-desktop must be installed beside the native host");
    }
    Ok(candidate)
}

/// Verify, stage, health check and activate. Ordering is the safety property:
/// nothing replaces the running build until the staged build has started and
/// answered the local protocol.
fn run_update(root: &Path, host: &str, manifest: Option<PathBuf>) -> anyhow::Result<()> {
    native::record_update_result(root, "agent", "running");
    let result = apply_update(root, host, manifest);
    native::record_update_result(
        root,
        "agent",
        if result.is_ok() {
            "completed"
        } else {
            "failed"
        },
    );
    let install = update::install_root(&std::env::current_exe()?);
    if result.is_ok() {
        let dir = hostreg::manifest_dir()?;
        let _ = update::prune(&install, host, &dir, 2);
    }
    result
}

fn apply_update(root: &Path, host: &str, manifest: Option<PathBuf>) -> anyhow::Result<()> {
    let config = session::config(root)?;
    let (source, public_key) = match (&config.update_source, &config.update_public_key) {
        (Some(source), Some(key)) => (source.clone(), key.clone()),
        _ => anyhow::bail!(
            "尚未配置签名更新源；请提供 --update-from <manifest> 或先设置更新源与公钥"
        ),
    };
    let bytes = match &manifest {
        Some(path) => std::fs::read(path)?,
        None => update::fetch_manifest(&source)?,
    };
    let release = update::parse_release(&bytes)?;
    update::verify(
        &release,
        &source,
        cloudfile_core::VERSION,
        cloudfile_core::now(),
        &update::platform_id(std::env::consts::OS, std::env::consts::ARCH),
        &public_key,
    )?;
    let os = std::env::consts::OS;
    let mut files = Vec::new();
    for artifact in &release.artifacts {
        let kind = match artifact.kind.as_str() {
            "native-host" => update::Binary::NativeHost,
            other => {
                anyhow::ensure!(other == "desktop", "unknown artifact");
                update::Binary::Desktop
            }
        };
        files.push((kind, update::download(artifact)?));
    }
    let install = update::install_root(&std::env::current_exe()?);
    let staged = update::stage(&install, &release.version, os, &files)?;
    let staged_host = staged.join(update::Binary::NativeHost.file_name(os, &release.version));
    let staged_desktop = staged.join(update::Binary::Desktop.file_name(os, &release.version));
    let healthy = update::probe(&staged_host)
        .and_then(|reported| {
            anyhow::ensure!(
                reported == release.version,
                "staged binary reports a different version"
            );
            update::probe_component(&staged_host).map(|_| ())
        })
        .and_then(|()| update::probe(&staged_desktop).map(|_| ()));
    if let Err(error) = healthy {
        // A staging directory that never activated is not installed state.
        let _ = std::fs::remove_dir_all(&staged);
        return Err(error);
    }
    let dir = hostreg::manifest_dir()?;
    update::activate(&install, &dir, host, &release.version)?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "version": release.version,
            "install_root": install.to_string_lossy(),
            "native_host": staged_host.to_string_lossy(),
        }))?
    );
    Ok(())
}

fn run_stdio() -> anyhow::Result<()> {
    let root = root()?;
    let mut input = std::io::stdin().lock();
    let mut output = std::io::stdout().lock();
    while let Some(value) = wire::read(&mut input)? {
        let response = match serde_json::from_value::<wire::Request>(value) {
            Ok(wire::Request::Status {}) => native::status(&root),
            Ok(wire::Request::Tasks {}) => native::task_summary(&root),
            Ok(wire::Request::CheckUpdate {}) => native::check_update(&root),
            Ok(wire::Request::Update {}) => start_detached("--update", &root, "agent"),
            Ok(wire::Request::UpdateExtension {}) => {
                start_detached("--update-extension", &root, "extension")
            }
            Ok(wire::Request::QueryLocalFile {
                repo_id,
                path,
                mode,
            }) => native::query_local_file(&root, &repo_id, &path, &mode),
            Ok(wire::Request::QueryLocalFolder { repo_id, mode }) => {
                native::query_local_folder(&root, &repo_id, &mode)
            }
            Ok(wire::Request::OpenWorkspace {
                repo_id,
                path,
                mode,
            }) => native::open_workspace(&root, &repo_id, &path, &mode),
            Ok(wire::Request::OpenSessionFile { path }) => native::open_session_file(&path),
            Ok(request) => forward(&root, request),
            Err(_) => wire::error("invalid or unsupported native message"),
        };
        wire::write(&mut output, &response)?;
    }
    Ok(())
}

/// Anything that needs the resident process goes over the mailbox, so the
/// window can be raised in the same place the user already looks.
fn forward(root: &Path, request: wire::Request) -> Value {
    match ipc::call(root, request.clone()) {
        Ok(value) => value,
        Err(_) => {
            // Pairing and direct .cloudfile opening are native-host work: they
            // must not depend on the desktop being alive.
            match request {
                wire::Request::PairEnrollment { server } => match device::enrollment(root, &server)
                {
                    Ok(value) => json!({"ok":true,"enrollment":value,
                        "service_path":session::service_path(root,&server).unwrap_or_default()}),
                    Err(error) => wire::error(&error.to_string()),
                },
                wire::Request::PairSign { server, response } => {
                    match device::pair_sign(root, &server, response) {
                        Ok(value) => json!({"ok":true,"proof":value}),
                        Err(error) => wire::error(&error.to_string()),
                    }
                }
                wire::Request::PairConfirm { server, response } => {
                    match device::confirm(root, &server, response) {
                        Ok(()) => json!({"ok":true}),
                        Err(error) => wire::error(&error.to_string()),
                    }
                }
                wire::Request::OpenUri { uri } => match paired_session::open(root, &uri) {
                    Ok(task) => json!({"ok":true,"task_id":task.task_id}),
                    Err(error) => wire::error(&error.to_string()),
                },
                _ => wire::error("desktop unavailable; inspect task center"),
            }
        }
    }
}

/// Upgrades must outlive this process: Chrome terminates the host as soon as
/// the response is written.
/// 把升级交给一个脱离宿主的自己。`kind` 决定需要什么配置：
/// Agent 自身升级要「源 + 公钥」二者齐备（否则无法验签），扩展升级只要源
/// （信任锚是同源 https + sha256），缺什么就在这里如实拒绝，别让扩展点了没反应。
fn start_detached(argument: &str, root: &Path, kind: &str) -> Value {
    if update_configured(root, kind).is_none() {
        return match kind {
            "extension" => wire::error("尚未配置更新源，无法更新扩展"),
            _ => wire::error("尚未配置签名更新源，无法从浏览器更新 Agent"),
        };
    }
    let Ok(exe) = std::env::current_exe() else {
        return wire::error("无法定位 Agent 程序");
    };
    let mut command = std::process::Command::new(exe);
    command
        .arg(argument)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        // DETACHED_PROCESS：这个子进程必须活过宿主进程（Chrome 写完应答就把宿主干掉），
        // 顺带也让它没有控制台，所以不需要 `childproc::hide`。
        command.creation_flags(0x0000_0008);
    }
    match command.spawn() {
        Ok(_) => {
            native::record_update_result(root, kind, "running");
            json!({"ok":true,"state":"running","last_update_state":"running","last_update_kind":kind})
        }
        Err(_) => {
            native::record_update_result(root, kind, "failed");
            json!({"ok":false,"error":"无法启动更新进程；当前版本保持不变"})
        }
    }
}

fn update_configured(root: &Path, kind: &str) -> Option<()> {
    let config = session::config(root).ok()?;
    let source = config.update_source?;
    if kind == "extension" {
        // 扩展升级不需要签名公钥，但它需要一个能解析出 archive 的相对源。
        return update::valid_source(&source).then_some(());
    }
    config.update_public_key.map(|_| ())
}
