//! Chrome native messaging host and the agent's own installer entry point.
//!
//! With no arguments this is a stdio host: Chrome writes length-prefixed JSON,
//! and every request is either answered locally or forwarded to the desktop
//! process over the file mailbox. With arguments it is a plain CLI, which is
//! how registration, login items, upgrades and reverts are performed without
//! a scripting runtime on the target machine.
use anyhow::Context;
use cloudfile_core::{
    apps, autostart, device, guard, hostreg, ipc, native, paired_session, selftest, session,
    storage, update, wire,
};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};

/// 桌面进程的文件名：安装时与 native host 同级，自检与更新都按它找。
const DESKTOP_NAME: &str = if cfg!(windows) {
    "cloudfile-desktop.exe"
} else {
    "cloudfile-desktop"
};

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
  --install                      install or repair this machine from the update channel
  --set-update-source <url>      configure the update channel (with --public-key)
  --clear-update-source          remove the configured update channel
  --check-update                 report the available signed release, if any
  --self-test                    check the local installation and exit (JSON, non-zero when broken)
  --update                       download, verify, stage and activate an update
  --update-from <manifest>       run an update from a local signed manifest
  --update-extension             install the newest browser extension and let it reload
  --rollback                     point the host back at the previous version
  --prune                        keep the active version plus one revert point

Install options:
  --source <url>                 primary update origin (probed first)
  --source-fallback <url>        mirror used when the primary is unreachable; the one
                                 that actually works is written back into the config
  --skip-registration            do not register the Chrome host
  --skip-autostart               do not add the login item
  --skip-launch                  do not start the task centre afterwards

Update options:
  --public-key <base64url>       pinned P-256 release key (required by https channels)
  --extension-dir <path>         local unpacked extension copy, for --update-extension
  --allow-insecure-source        allow a plain-http channel (development only; recorded
                                 in config.json and never enabled implicitly)

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
    // 默认就是本平台的 Host（Windows 上是 `com.cloudfile.current_agent`，见 hostreg::HOST）。
    // `--host-name` 只用于显式覆盖（例如按开发身份另注册一份、或清理旧的 dev host）。
    let host = flag(args, "--host-name").unwrap_or_else(|| hostreg::HOST.to_string());
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
        "--install" => run_install(&root, &host, args)?,
        "--set-update-source" => {
            let source = flag(args, "--set-update-source")
                .ok_or_else(|| anyhow::anyhow!("--set-update-source requires a url"))?;
            let public_key = flag(args, "--public-key");
            let extension_dir = flag(args, "--extension-dir");
            // 明文 http 只能靠这个开关放行，而且它会被记进 config.json ——
            // 以后每次升级读回来的是同一条规则，不会出现「设置时宽松、升级时严格」。
            let policy = if args.iter().any(|value| value == "--allow-insecure-source") {
                update::SourcePolicy::DEV
            } else {
                update::SourcePolicy::STRICT
            };
            session::set_update_with_policy(
                &root,
                Some(source),
                public_key,
                extension_dir,
                policy,
            )?;
            let config = session::config(&root)?;
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "update_source": config.update_source,
                    "update_public_key_set": config.update_public_key.is_some(),
                    "allow_insecure_update_source": config.allow_insecure_update_source,
                    "extension_dir": config.extension_dir,
                }))?
            );
        }
        "--clear-update-source" => {
            session::set_update(&root, None, None, None)?;
            println!("update source cleared");
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
        "--self-test" => {
            // 「搭档」是桌面进程：Agent 能跑，但用户没有入口。装的时候两个一起装，
            // 所以缺了就该报出来（读不到路径时用文件名占位，报告里会指出找不到）。
            let peer = desktop_binary().unwrap_or_else(|_| PathBuf::from(DESKTOP_NAME));
            let report = selftest::run(&root, &peer);
            println!("{}", serde_json::to_string_pretty(&report)?);
            if report["ok"] != json!(true) {
                // 退出码要能被脚本判：0 = 全绿，1 = 有项确实不工作。
                std::process::exit(1);
            }
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
    let name = DESKTOP_NAME;
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
    let (release, _, _) = fetch_verified_release(root, manifest.as_deref())?;
    apply_release(host, &release)
}

/// 取清单并验签，返回 (清单, 策略, 源)。`--install` 复用这一段，所以它不碰安装目录。
fn fetch_verified_release(
    root: &Path,
    manifest: Option<&Path>,
) -> anyhow::Result<(update::Release, update::SourcePolicy, String)> {
    let config = session::config(root)?;
    let (source, public_key) = match (&config.update_source, &config.update_public_key) {
        (Some(source), Some(key)) => (source.clone(), key.clone()),
        _ => anyhow::bail!(
            "尚未配置签名更新源；请提供 --update-from <manifest> 或先设置更新源与公钥"
        ),
    };
    let policy = config.update_policy();
    let bytes = match manifest {
        Some(path) => std::fs::read(path)?,
        None => update::fetch_manifest_for(policy, &source)?,
    };
    let release = update::parse_release(&bytes)?;
    update::verify(
        policy,
        &release,
        &source,
        cloudfile_core::VERSION,
        cloudfile_core::now(),
        &update::platform_id(std::env::consts::OS, std::env::consts::ARCH),
        &public_key,
    )?;
    Ok((release, policy, source))
}

/// 一键安装 / 修复。等价于 Go 的 `install.cmd`，区别只是目标机器**不需要任何脚本运行时**。
///
/// 步骤与顺序：安装目录可写 → 定源并写回（主源不通退备源）→ 有更新才换二进制 →
/// 注册 Host → 设开机自启 → 启动任务中心。幂等：已经装好的机器重跑一遍等于修复
/// （注册与自启重做、二进制不动）。
fn run_install(root: &Path, host: &str, args: &[String]) -> anyhow::Result<()> {
    // 先判「装得进去」再下载：别下完几 MB 才发现目录不可写（Go 的 :usable 也是这个顺序）。
    let install = update::install_root(&std::env::current_exe()?);
    ensure_install_root(&install)?;

    let (source, policy, probes) = resolve_source(root, args)?;
    let dir = hostreg::manifest_dir()?;

    // 二进制：清单里有更新才暂存。暂存只写版本目录、**不碰注册** —— 「健康检查通过之前
    // 什么都不替换」这条安全顺序仍然成立；而全新安装这边没有旧注册可以接管
    // （`update::activate` 是「接管既有注册」专用的），所以注册必须能和激活分开做。
    let (binaries, staged) = stage_binaries(root)?;

    // 注册 Host 是安装的实质产物：没有它扩展就找不到 Agent。
    let (registration, registered_dir) = if args.iter().any(|value| value == "--skip-registration")
    {
        (
            json!({"skipped": true}),
            host_binary()?.parent().map(Path::to_path_buf),
        )
    } else {
        let (target, extension_id, takeover) =
            registration_target(&dir, host, staged.as_ref(), args)?;
        let outcome = hostreg::install(&dir, host, &extension_id, &target, takeover).map_err(
            |error| {
                anyhow::anyhow!(
                    "{error}\n\n如果确认这属于本机 Agent，请加 --takeover 重新注册；原注册会先备份并可用 --rollback-host 还原"
                )
            },
        )?;
        let directory = target.parent().map(Path::to_path_buf);
        (serde_json::to_value(outcome)?, directory)
    };

    // 自启与启动都用**注册真正指向的那个目录**：全新安装时正在跑的可能只是引导下载的临时
    // 副本，它旁边没有桌面进程。失败都只警告 —— 二进制已经就位，用户能自己双击。
    let desktop = registered_dir
        .as_deref()
        .map(|directory| directory.join(DESKTOP_NAME));
    let login_item = if args.iter().any(|value| value == "--skip-autostart") {
        None
    } else {
        match desktop.as_deref().filter(|path| path.is_file()) {
            Some(desktop) => match autostart::install(root, desktop) {
                Ok(item) => Some(item.id().to_string()),
                Err(error) => {
                    eprintln!("警告：开机自启没设上（{error:#}）；重启后手动双击任务中心即可。");
                    None
                }
            },
            None => {
                eprintln!("警告：注册目录里没有 {DESKTOP_NAME}，开机自启跳过。");
                None
            }
        }
    };

    let launched = if args.iter().any(|value| value == "--skip-launch") {
        false
    } else {
        launch_desktop(desktop.as_deref())?
    };

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "version": cloudfile_core::VERSION,
            "install_root": install.to_string_lossy(),
            "update_source": source,
            "allow_insecure_update_source": policy.allow_insecure,
            "source_probes": probes.iter().map(|probe| json!({
                "source": probe.source,
                "error": probe.error,
            })).collect::<Vec<_>>(),
            "binaries": binaries,
            "staged_version": staged.as_ref().map(|(version, _)| version.clone()),
            "registered_directory": registered_dir
                .as_deref()
                .map(|dir| dir.to_string_lossy().into_owned()),
            "registration": registration,
            "login_item": login_item,
            "desktop_started": launched,
        }))?
    );
    Ok(())
}

/// 安装目录能不能真写进去：建目录 + 写一个探针文件再删掉。
fn ensure_install_root(install: &Path) -> anyhow::Result<()> {
    std::fs::create_dir_all(install)
        .with_context(|| format!("创建安装目录失败：{}", install.display()))?;
    let probe = install.join(".cf-write-test");
    std::fs::write(&probe, b"ok")
        .with_context(|| format!("安装目录不可写：{}", install.display()))?;
    let _ = std::fs::remove_file(&probe);
    Ok(())
}

/// 定源并**把它写回配置**。
///
/// 给了 `--source` 就依次探测（`--source-fallback` 是备用、通常是直连静态服务的 IP），
/// 写回的是这台机器**真的能访问**的那个 —— 否则用户装完、第一次升级时才发现主源
/// 在这台机器上根本不通。两个都不通时，报错里带着每个地址各自的原因。
fn resolve_source(
    root: &Path,
    args: &[String],
) -> anyhow::Result<(String, update::SourcePolicy, Vec<update::SourceProbe>)> {
    let policy = if args.iter().any(|value| value == "--allow-insecure-source") {
        update::SourcePolicy::DEV
    } else {
        update::SourcePolicy::STRICT
    };
    let mut candidates = Vec::new();
    if let Some(primary) = flag(args, "--source") {
        candidates.push(primary);
    }
    if let Some(fallback) = flag(args, "--source-fallback") {
        candidates.push(fallback);
    }
    if candidates.is_empty() {
        // 没给地址：用这台机器上已经配好的（策略也以配置为准，见 --set-update-source）。
        let config = session::config(root)?;
        // 先算策略：下面的 ok_or_else 会把 update_source 移走。
        let policy = config.update_policy();
        let source = config.update_source.ok_or_else(|| {
            anyhow::anyhow!(
                "--install 需要 --source <url>，或先用 --set-update-source 配置更新通道"
            )
        })?;
        return Ok((source, policy, Vec::new()));
    }

    let (picked, probes) = update::pick_source(policy, &candidates);
    let Some(source) = picked else {
        anyhow::bail!(
            "两个更新源都不可用：{}\n请检查本机能否访问上面的地址（DNS/代理/防火墙）后重试。",
            update::describe_probes(&probes)
        );
    };
    session::set_update_with_policy(
        root,
        Some(source.clone()),
        flag(args, "--public-key"),
        flag(args, "--extension-dir"),
        policy,
    )?;
    Ok((source, policy, probes))
}

/// 二进制：清单里有更新才暂存，已是最新就跳过（`--install` 同时是修复命令）。
/// 返回 (结论, 暂存好的 (版本, host 路径))。
fn stage_binaries(root: &Path) -> anyhow::Result<(&'static str, Option<(String, PathBuf)>)> {
    let config = session::config(root)?;
    let (Some(source), Some(_)) = (
        config.update_source.clone(),
        config.update_public_key.as_ref(),
    ) else {
        // 只有扩展通道（开发通道常见）：二进制不升级，但安装照样可以完成。
        return Ok(("skipped", None));
    };
    let policy = config.update_policy();
    // 先**不验签**地读一眼版本号：验签本身会拒绝「不比当前新」的清单，而安装器必须先知道
    // 有没有更新才能走幂等（修复）那条路。签名通过之前不会下载任何字节。
    let advertised = update::parse_release(&update::fetch_manifest_for(policy, &source)?)?;
    if update::compare(&advertised.version, cloudfile_core::VERSION) != std::cmp::Ordering::Greater
    {
        return Ok(("current", None));
    }
    // 同一份清单再取一次并完整验签。清单只有几十 KB、一台机器只跑一次安装，
    // 不值得为了省这一次请求把「验签」拆到两个地方。
    let (release, _, _) = fetch_verified_release(root, None)?;
    let (_, staged_host) = stage_release(&release)?;
    Ok(("staged", Some((release.version.clone(), staged_host))))
}

/// 注册该指向哪个二进制、用哪个扩展 ID、要不要接管。顺序就是优先级：
///
/// 1. 这次刚暂存了新版本 → 指它（升级与全新安装都指它）
/// 2. 否则已有注册 → 沿用它（不无缘无故改别人的注册；`--extension-id` 显式给了才换）
/// 3. 都没有 → 就地修复：指当前正在跑的这份
fn registration_target(
    dir: &Path,
    host: &str,
    staged: Option<&(String, PathBuf)>,
    args: &[String],
) -> anyhow::Result<(PathBuf, String, bool)> {
    let existing = hostreg::state(dir, host, None)?;
    let target = match staged {
        // 暂存那一步返回的就是 host 的最终路径（版本目录里的那个），直接注册它。
        Some((_, staged_host)) => staged_host.clone(),
        None => match &existing.binary {
            Some(binary) => binary.clone(),
            None => host_binary()?,
        },
    };
    // 已有注册时沿用它的扩展 ID：白名单不该因为重跑一次安装就被改掉。
    let extension_id = flag(args, "--extension-id")
        .or_else(|| existing.extension_id.clone())
        .ok_or_else(|| {
            anyhow::anyhow!(
                "--install 需要 --extension-id <id>：本机还没有可接管的注册，\n\
                 扩展 ID 只能由安装方给出（它决定「哪个扩展能调用本机 Agent」）"
            )
        })?;
    // 有旧注册、或这次换了版本，都要显式接管：`hostreg::install` 会先把原 manifest
    // 备份成 `<host>.previous.json`，可用 `--rollback-host` 还原。
    let takeover = staged.is_some()
        || existing.binary.is_some()
        || args.iter().any(|value| value == "--takeover");
    Ok((target, extension_id, takeover))
}

/// 装完立刻启动任务中心：否则用户重启之前右下角什么都没有。
/// 桌面是 GUI 子系统程序，读写的都是自己的私有目录，启动它不会弹控制台。
fn launch_desktop(desktop: Option<&Path>) -> anyhow::Result<bool> {
    let Some(desktop) = desktop.filter(|path| path.is_file()) else {
        eprintln!("警告：找不到 {DESKTOP_NAME}，任务中心没能自动启动；装好后手动双击即可。");
        return Ok(false);
    };
    let mut command = std::process::Command::new(desktop);
    command
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    cloudfile_core::childproc::hide(&mut command);
    match command.spawn() {
        Ok(_) => Ok(true),
        Err(error) => {
            // 启动失败不算安装失败：二进制已经就位，用户可以自己双击。
            eprintln!(
                "警告：任务中心没能自动启动（{error}）；请手动双击 {}",
                desktop.display()
            );
            Ok(false)
        }
    }
}

/// 下载、暂存、健康检查、激活。顺序就是安全属性：在暂存的构建起来并回答本地协议之前，
/// 什么都替换不了正在运行的那份。
fn apply_release(host: &str, release: &update::Release) -> anyhow::Result<()> {
    let (install, staged_host) = stage_release(release)?;
    // `activate` 是「接管既有注册」专用的：它从当前注册取扩展 ID、把注册指到新版本目录，
    // 并备份原 manifest。全新安装没有旧注册可接管（会明确报错），那条路走 `--install`。
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

/// 下载 + 暂存 + 健康检查，返回 (安装根, 暂存好的 host 路径)。**不碰注册** ——
/// 全新安装这边没有旧注册可以接管，所以这一步必须能单独用。
fn stage_release(release: &update::Release) -> anyhow::Result<(PathBuf, PathBuf)> {
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
    // 健康检查：新构建必须能起来并回答本地协议，否则一律不许留在版本目录里。
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
    Ok((install, staged_host))
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
    // 先算策略：`update_source?` 会把该字段移走。
    let policy = config.update_policy();
    let source = config.update_source?;
    if kind == "extension" {
        // 扩展升级不需要签名公钥，但它需要一个能解析出 archive 的相对源 ——
        // scheme 的判定走这台机器上已记录的策略（正式通道 https，开发通道也许放行了 http）。
        return policy.valid_source(&source).then_some(());
    }
    // Agent 自身升级要公钥（验签用），扩展不需要。
    config.update_public_key.as_ref()?;
    policy.valid_source(&source).then_some(())
}
