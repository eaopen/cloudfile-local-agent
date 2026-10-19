//! End-to-end contract for the shipped installer CLI.
//!
//! Registration, autostart and revert are the parts of the 2.2 plan that unit
//! tests cannot prove: they depend on the real executable, the real per-user
//! directories and the platform's host manifest layout. Every case below runs
//! the actual binary with `HOME` (and on Windows `LOCALAPPDATA`) redirected to
//! a temporary directory, so the developer's own Chrome registration and private
//! store are never touched.
use serde_json::{json, Value};
use std::path::{Path, PathBuf};
use std::process::Command;

const VERSION: &str = env!("CARGO_PKG_VERSION");
const EXTENSION_ID: &str = "abcdefghijklmnopabcdefghijklmnop";
const DEV_HOST: &str = "com.cloudfile.agent22.dev";

/// A sandboxed user profile. `canonicalize` matters: the private store refuses
/// symlinked ancestors, and the platform temporary directory is reached through
/// one on macOS.
struct Sandbox {
    _directory: tempfile::TempDir,
    home: PathBuf,
    local_app_data: Option<PathBuf>,
}

impl Sandbox {
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let home = directory.path().canonicalize().unwrap();
        std::fs::create_dir_all(home.join("Library/Application Support")).unwrap();
        std::fs::create_dir_all(home.join("AppData/Local")).unwrap();
        let local_app_data = cfg!(windows).then(|| home.join("AppData/Local"));
        Sandbox {
            _directory: directory,
            home,
            local_app_data,
        }
    }

    fn command(&self) -> Command {
        self.command_at(Path::new(env!("CARGO_BIN_EXE_cloudfile-native-host")))
    }

    /// 同一套沙箱环境，但跑的是**另一个位置**的二进制（例如复制到孤立目录的副本，
    /// 用来复现「旁边没有桌面进程」的半截安装）。
    fn command_at(&self, binary: &Path) -> Command {
        let mut command = Command::new(binary);
        command.env("HOME", &self.home);
        command.env("USERPROFILE", &self.home);
        if let Some(local) = &self.local_app_data {
            command.env("LOCALAPPDATA", local);
        }
        command
    }

    /// Runs a CLI command and returns stdout, asserting it succeeded.
    fn run(&self, arguments: &[&str]) -> String {
        let output = self.command().args(arguments).output().unwrap();
        assert!(
            output.status.success(),
            "{arguments:?} failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_string()
    }

    /// Runs a CLI command expecting a refusal, returning stderr.
    fn refuses(&self, arguments: &[&str]) -> String {
        let output = self.command().args(arguments).output().unwrap();
        assert!(
            !output.status.success(),
            "{arguments:?} unexpectedly succeeded"
        );
        String::from_utf8_lossy(&output.stderr).trim().to_string()
    }

    fn run_json(&self, arguments: &[&str]) -> Value {
        serde_json::from_str(&self.run(arguments)).unwrap()
    }

    /// 沙箱里的 config.json（Agent 的更新通道与扩展目录都记在这里）。
    fn config_path(&self) -> PathBuf {
        self.local_app_data
            .clone()
            .unwrap()
            .join("CloudFileAgent22/config.json")
    }

    fn manifest_dir(&self) -> PathBuf {
        if cfg!(windows) {
            self.local_app_data
                .clone()
                .unwrap()
                .join("CloudFileAgent22/native-host")
        } else {
            self.home
                .join("Library/Application Support/Google/Chrome/NativeMessagingHosts")
        }
    }

    fn manifest(&self) -> PathBuf {
        self.manifest_dir().join(format!("{DEV_HOST}.json"))
    }

    /// Removes anything a failed assertion left behind, so a broken run cannot
    /// disguise itself as a passing one on the next attempt.
    fn cleanup(&self) {
        let _ = self.command().args(["--unregister-host"]).output();
    }
}

#[test]
fn version_and_component_are_machine_readable() {
    let sandbox = Sandbox::new();
    assert_eq!(sandbox.run(&["--version"]), VERSION);

    let component = sandbox.run_json(&["--component"]);
    assert_eq!(component["version"], VERSION);
    assert_eq!(component["agent_family"], "agent2");
    // The extension gates its own behaviour on these two numbers, so a build
    // that cannot state them is not a usable component.
    assert!(component["ipc_version"].as_u64().unwrap() > 0);
    assert!(component["journal_version"].as_u64().unwrap() > 0);
    let platform = component["platform"].as_str().unwrap();
    assert!(platform.starts_with(std::env::consts::OS), "{platform}");
    assert!(platform.contains(std::env::consts::ARCH), "{platform}");
}

#[test]
fn usage_is_printed_for_help_and_for_an_unknown_command() {
    let sandbox = Sandbox::new();
    let help = sandbox.run(&["--help"]);
    for expected in [
        "--register-host",
        "--unregister-host",
        "--rollback-host",
        "--enable-autostart",
        "--update",
        "--rollback",
    ] {
        assert!(help.contains(expected), "{expected} missing from usage");
    }
    // An unknown command must teach the caller rather than fail blankly.
    let unknown = sandbox.refuses(&["--definitely-not-a-command"]);
    assert!(unknown.contains("unknown command"), "{unknown}");
    assert!(unknown.contains("--register-host"), "{unknown}");
}

#[test]
fn registration_round_trip_writes_a_chrome_manifest_and_reverts() {
    let sandbox = Sandbox::new();
    let manifest = sandbox.manifest();
    let previous = sandbox
        .manifest_dir()
        .join(format!("{DEV_HOST}.previous.json"));

    assert!(!manifest.exists());
    // Nothing to undo yet: a revert is refused rather than silently ignored.
    assert!(sandbox
        .refuses(&["--rollback-host"])
        .contains("no previous"));

    let outcome = sandbox.run_json(&[
        "--register-host",
        "--extension-id",
        EXTENSION_ID,
        "--host-name",
        DEV_HOST,
    ]);
    assert_eq!(outcome["host"], DEV_HOST);
    assert_eq!(outcome["backed_up"], false);

    let written: Value =
        serde_json::from_str(&std::fs::read_to_string(&manifest).unwrap()).unwrap();
    assert_eq!(written["name"], DEV_HOST);
    assert_eq!(written["type"], "stdio");
    assert_eq!(
        written["allowed_origins"][0],
        format!("chrome-extension://{EXTENSION_ID}/")
    );
    // Chrome cannot launch a relative interpreter, so the path must be absolute
    // and must be this very executable.
    let path = Path::new(written["path"].as_str().unwrap());
    assert!(path.is_absolute(), "{path:?}");
    assert_eq!(
        path.file_name().unwrap().to_str().unwrap(),
        if cfg!(windows) {
            "cloudfile-native-host.exe"
        } else {
            "cloudfile-native-host"
        }
    );
    assert!(path.is_file(), "{path:?}");

    let state = sandbox.run_json(&["--host-state", "--host-name", DEV_HOST]);
    assert_eq!(state["registered"], true);
    assert_eq!(state["foreign"], false);
    assert_eq!(state["extension_id"], EXTENSION_ID);

    // Re-registering with a different extension backs the old manifest up.
    let second_id = "ponmlkjihgfedcbaponmlkjihgfedcba";
    let second = sandbox.run_json(&[
        "--register-host",
        "--extension-id",
        second_id,
        "--host-name",
        DEV_HOST,
    ]);
    assert_eq!(second["backed_up"], true);
    assert!(previous.is_file());

    // A revert reports the manifest it restored, which stays where Chrome reads it.
    let restored = sandbox.run(&["--rollback-host", "--host-name", DEV_HOST]);
    assert_eq!(PathBuf::from(restored), manifest);
    let rolled_back: Value =
        serde_json::from_str(&std::fs::read_to_string(&manifest).unwrap()).unwrap();
    assert_eq!(
        rolled_back["allowed_origins"][0],
        format!("chrome-extension://{EXTENSION_ID}/")
    );

    let removed = sandbox.run(&["--unregister-host", "--host-name", DEV_HOST]);
    assert_eq!(PathBuf::from(removed), manifest);
    assert!(!manifest.exists());
    // Idempotent: unregistering an absent host is a no-op, not an error.
    sandbox.run(&["--unregister-host", "--host-name", DEV_HOST]);
}

#[test]
fn a_foreign_registration_is_never_replaced_silently() {
    let sandbox = Sandbox::new();
    let manifest = sandbox.manifest();
    std::fs::create_dir_all(sandbox.manifest_dir()).unwrap();
    let foreign = serde_json::json!({
        "name": DEV_HOST,
        "description": "Some Other Vendor Host",
        "path": "/opt/other-vendor/host",
        "type": "stdio",
        "allowed_origins": ["chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/"],
    });
    std::fs::write(&manifest, foreign.to_string()).unwrap();

    let refusal = sandbox.refuses(&[
        "--register-host",
        "--extension-id",
        EXTENSION_ID,
        "--host-name",
        DEV_HOST,
    ]);
    assert!(refusal.contains("another program"), "{refusal}");
    // The other vendor's manifest is untouched by the refusal.
    let still: Value = serde_json::from_str(&std::fs::read_to_string(&manifest).unwrap()).unwrap();
    assert_eq!(still["path"], "/opt/other-vendor/host");
    // And it is reported as foreign rather than as a registration of ours.
    assert_eq!(
        sandbox.run_json(&["--host-state", "--host-name", DEV_HOST])["foreign"],
        true
    );
    // Unregister must not remove a host this agent did not write.
    assert!(sandbox
        .refuses(&["--unregister-host", "--host-name", DEV_HOST])
        .contains("not registered by this agent"));
    assert!(manifest.exists());

    // Replacing it is allowed once the caller asks for it in so many words, and
    // the other vendor's manifest is preserved as the named revert point.
    let taken = sandbox.run_json(&[
        "--register-host",
        "--extension-id",
        EXTENSION_ID,
        "--host-name",
        DEV_HOST,
        "--takeover",
    ]);
    assert_eq!(taken["backed_up"], true);
    let previous = PathBuf::from(taken["previous_manifest_path"].as_str().unwrap());
    let preserved: Value =
        serde_json::from_str(&std::fs::read_to_string(&previous).unwrap()).unwrap();
    assert_eq!(preserved["path"], "/opt/other-vendor/host");
    assert_eq!(
        sandbox.run_json(&["--host-state", "--host-name", DEV_HOST])["foreign"],
        false
    );
    // And it can be handed straight back.
    sandbox.run(&["--rollback-host", "--host-name", DEV_HOST]);
    let handed_back: Value =
        serde_json::from_str(&std::fs::read_to_string(&manifest).unwrap()).unwrap();
    assert_eq!(handed_back["path"], "/opt/other-vendor/host");
    assert!(sandbox
        .refuses(&["--unregister-host", "--host-name", DEV_HOST])
        .contains("not registered by this agent"));
}

#[test]
fn an_invalid_extension_id_is_rejected_before_anything_is_written() {
    let sandbox = Sandbox::new();
    // Chrome only ever issues [a-p]{32}; anything else cannot be honoured.
    for candidate in ["", "short", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"] {
        let refusal = sandbox.refuses(&[
            "--register-host",
            "--extension-id",
            candidate,
            "--host-name",
            DEV_HOST,
        ]);
        assert!(!refusal.is_empty());
        assert!(!sandbox.manifest().exists(), "{candidate} wrote a manifest");
    }
    // 不传 ID 时用产品常量：扩展 ID 固定，安装不该要求用户抄一串字符。
    let outcome = sandbox.run_json(&["--register-host", "--host-name", DEV_HOST]);
    assert_eq!(outcome["host"], DEV_HOST);
    let written: Value =
        serde_json::from_str(&std::fs::read_to_string(sandbox.manifest()).unwrap()).unwrap();
    assert_eq!(
        written["allowed_origins"][0],
        format!("chrome-extension://{}/", cloudfile_core::EXTENSION_ID)
    );
    sandbox.run(&["--unregister-host", "--host-name", DEV_HOST]);
}

#[test]
fn status_reports_the_local_component_contract() {
    let sandbox = Sandbox::new();
    let status = sandbox.run_json(&["--status", "--host-name", DEV_HOST]);
    assert_eq!(status["version"], VERSION);
    assert_eq!(status["host"], DEV_HOST);
    // A fresh profile has no registration, but the report must still be whole.
    assert_eq!(status["registered"], false);
    assert_eq!(status["foreign"], false);
    // 登录项是**真实**的用户级状态（HKLM/HKCU 不随 HOME 变），开发机上可能本来就开着。
    // 这里断言的是两个字段不能互相矛盾，而不是「这台机器一定没开自启」。
    assert_eq!(
        status["autostart"],
        json!(status["autostart_mechanism"].is_string())
    );
    assert_eq!(status["exit"]["safe_to_exit"], true);
    let install_root = Path::new(status["install_root"].as_str().unwrap());
    assert!(install_root.is_absolute(), "{install_root:?}");
    assert!(status["applications"].is_array());
    assert!(status["installed_versions"].is_array());
    // The desktop shell's platform runtime is reported, never assumed. macOS
    // renders through the system WebKit, so the field is null there.
    if cfg!(windows) {
        assert!(
            status["runtime"]["webview2"].is_string() || status["runtime"]["webview2"].is_null(),
            "{status}"
        );
    } else {
        assert_eq!(status["runtime"]["webview2"], Value::Null);
    }
    // No manifest yet, so nothing is active. The extension reads this field
    // directly, so an absent version is a JSON null rather than a missing key.
    assert_eq!(status["active_version"], Value::Null);

    sandbox.cleanup();
}

/// 最小 HTTP 服务：给每个请求回固定 body。用来确定性地验证「主源可用时不再碰备源」——
/// 这条特性（对齐 Go install.cmd 的主备地址）不能靠局域网碰运气来验。
fn serve(body: &'static [u8]) -> String {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    std::thread::spawn(move || {
        for stream in listener.incoming().take(4) {
            let Ok(mut stream) = stream else { continue };
            let mut buffer = [0u8; 8192];
            let _ = std::io::Read::read(&mut stream, &mut buffer);
            let header = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            let _ = std::io::Write::write_all(&mut stream, header.as_bytes());
            let _ = std::io::Write::write_all(&mut stream, body);
        }
    });
    format!("http://127.0.0.1:{port}")
}

#[test]
fn the_private_root_can_be_moved_with_the_documented_variable() {
    let sandbox = Sandbox::new();
    // 现场常把 Agent 装到别的盘；install.cmd 一直有这个变量，Rust 侧现在也认它。
    let elsewhere = sandbox.local_app_data.clone().unwrap().join("Moved22");
    let output = sandbox
        .command()
        .env("CLOUDFILE_AGENT2_HOME", &elsewhere)
        .args(["--self-test"])
        .output()
        .unwrap();
    let report: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(report["root"], elsewhere.to_string_lossy().as_ref());
    assert!(elsewhere.is_dir(), "被指定的根目录应当被创建");
    // 相对路径一律拒绝：那会让「装到哪」取决于当前目录。
    let relative = sandbox
        .command()
        .env("CLOUDFILE_AGENT2_HOME", "relative-home")
        .args(["--self-test"])
        .output()
        .unwrap();
    assert!(!relative.status.success());
    assert!(
        String::from_utf8_lossy(&relative.stderr).contains("absolute path"),
        "{}",
        String::from_utf8_lossy(&relative.stderr)
    );
    sandbox.cleanup();
}

#[test]
fn the_installer_needs_a_source_before_it_touches_anything() {
    let sandbox = Sandbox::new();
    let refusal = sandbox.refuses(&["--install"]);
    assert!(refusal.contains("--source"), "{refusal}");
    // 什么都没配：不该有任何 source 被写进去。
    assert!(!sandbox.config_path().exists(), "配置不该被创建");
    sandbox.cleanup();
}

#[test]
fn the_installer_reports_every_source_it_tried() {
    let sandbox = Sandbox::new();
    // 两个都不通（loopback 上没人监听：立刻失败，不用等超时）。
    let refusal = sandbox.refuses(&[
        "--install",
        "--source",
        "http://127.0.0.1:1/primary",
        "--source-fallback",
        "http://127.0.0.1:2/mirror",
        "--allow-insecure-source",
    ]);
    // 现场要看到「试过谁、为什么不行」，而不是一句「安装失败」。
    assert!(refusal.contains("http://127.0.0.1:1/primary"), "{refusal}");
    assert!(refusal.contains("http://127.0.0.1:2/mirror"), "{refusal}");
    assert!(refusal.contains("不可用"), "{refusal}");
    sandbox.cleanup();
}

#[test]
fn the_installer_keeps_the_first_usable_source_and_writes_it_back() {
    let sandbox = Sandbox::new();
    // 主源可用（回环服务），备源故意指向没人监听的端口。
    let primary = serve(br#"{"version":"0.0.0"}"#);
    // 跳过注册/自启/启动：它们会碰真实的注册表与登录项，与本用例要验的事无关。
    let output = sandbox
        .command()
        .args([
            "--install",
            "--source",
            &primary,
            "--source-fallback",
            "http://127.0.0.1:1/mirror",
            "--allow-insecure-source",
            "--skip-registration",
            "--skip-autostart",
            "--skip-launch",
        ])
        .output()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout);
    let report: Value = serde_json::from_str(&stdout).unwrap_or_else(|error| {
        panic!(
            "{error}: {stdout}\n{}",
            String::from_utf8_lossy(&output.stderr)
        )
    });
    assert_eq!(report["update_source"], primary.as_str());
    // 备源**一次都没被探测**：探测列表里只有主源，而且它是可用的。
    let probes = report["source_probes"].as_array().unwrap();
    assert_eq!(probes.len(), 1, "{report}");
    assert_eq!(probes[0]["source"], primary.as_str());
    assert_eq!(probes[0]["error"], Value::Null);
    // 没有签名公钥（开发通道）：二进制不升级，但安装照样完成。
    assert_eq!(report["binaries"], "skipped");
    assert!(output.status.success(), "{stdout}");

    // 实际可用的地址被写回了配置：以后升级不用再猜。
    let config: Value =
        serde_json::from_str(&std::fs::read_to_string(sandbox.config_path()).unwrap()).unwrap();
    assert_eq!(config["update_source"], primary.as_str());
    assert_eq!(config["allow_insecure_update_source"], true);
    sandbox.cleanup();
}

#[test]
fn the_self_test_reports_every_check_and_exits_non_zero_when_something_is_broken() {
    let sandbox = Sandbox::new();
    // 把 native host 复制到一个孤立目录里跑：它旁边没有桌面进程，等于「装了半个」。
    // 这样「坏掉时退出码非 0」是确定性可复现的，不依赖构建目录里恰好有没有那个二进制。
    let isolated = tempfile::tempdir().unwrap();
    let copy = isolated.path().join(if cfg!(windows) {
        "cloudfile-native-host.exe"
    } else {
        "cloudfile-native-host"
    });
    std::fs::copy(env!("CARGO_BIN_EXE_cloudfile-native-host"), &copy).unwrap();
    let output = sandbox
        .command_at(&copy)
        .args(["--self-test"])
        .output()
        .expect("self-test did not run");
    // 报告走 stdout（调用方按 stdout 解析），退出码用来给脚本判。
    let report: Value =
        serde_json::from_slice(&output.stdout).expect("self-test output is not JSON");
    assert!(
        output.stderr.is_empty(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        output.status.code(),
        Some(1),
        "半截安装必须报成不健康：{report}"
    );
    assert_eq!(report["ok"], false);

    let checks = report["checks"].as_array().unwrap();
    let names: Vec<&str> = checks
        .iter()
        .map(|check| check["name"].as_str().unwrap())
        .collect();
    // 名字是给现场排查用的接口，改名要同步改这里。
    for expected in [
        "private-store",
        "mailbox",
        "single-instance",
        "peer-binary",
        "component",
        "registration",
        "task-store",
        "exit-assessment",
        "webview2",
        "extension",
        "login-item",
        "update-source",
    ] {
        assert!(
            names.contains(&expected),
            "{expected} missing from {names:?}"
        );
    }

    let find = |name: &str| {
        checks
            .iter()
            .find(|check| check["name"] == name)
            .unwrap_or_else(|| panic!("{name} missing"))
            .clone()
    };
    // 缺的就是搭档程序，而且原因要带上文件名，不能只说「失败」。
    assert_eq!(find("peer-binary")["ok"], false);
    assert!(find("peer-binary")["detail"]
        .as_str()
        .unwrap()
        .contains("cloudfile-desktop"));
    // 私有目录那一项在沙箱里必须真的通（写进去又删掉）。
    assert_eq!(find("private-store")["ok"], true);
    // 从没配过更新源：如实报「未配置」，不算失败。
    assert_eq!(find("update-source")["detail"], "未配置");
    // 探针文件不许留在沙箱里。
    let root = sandbox
        .local_app_data
        .clone()
        .unwrap()
        .join("CloudFileAgent22");
    assert!(!root.join("self-test.json").exists());
    assert!(!root.join("self-test.lock").exists());

    // 原地跑一次：退出码必须与报告一致。写死 0 或 1 都会在某些机器上误判 ——
    // 搭档程序在不在取决于构建目录里有没有它。
    let output = sandbox.command().args(["--self-test"]).output().unwrap();
    let report: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(
        output.status.code(),
        Some(i32::from(report["ok"] != true)),
        "{report}"
    );

    sandbox.cleanup();
}

#[test]
fn a_plain_http_channel_is_accepted_but_still_needs_a_pinned_key() {
    let sandbox = Sandbox::new();
    // 明文不再是拒绝理由 —— 内网门户只有 http。卡住的是另一条规则：
    // 「有源、没签名公钥」的通道只服务扩展更新，Agent 自身不可升级，默认不接受。
    let refusal = sandbox.refuses(&[
        "--set-update-source",
        "http://10.9.8.162:6111/cloudfile-updates-dev",
    ]);
    assert!(refusal.contains("pinned release key"), "{refusal}");
    assert_eq!(
        sandbox.run_json(&["--check-update"])["update_source_set"],
        false
    );

    // 显式放行：写进去，并且「允许明文」这件事本身要能被读出来（审计要看的就是它）。
    // 用 127.0.0.1:1 而不是真实局域网地址：这条用例不该依赖网络，而且它必然立刻失败。
    let saved = sandbox.run_json(&[
        "--set-update-source",
        "http://127.0.0.1:1/dev",
        "--allow-insecure-source",
    ]);
    assert_eq!(saved["allow_insecure_update_source"], true);
    assert_eq!(saved["update_source"], "http://127.0.0.1:1/dev");
    // 开发通道允许「有源没公钥」：那种通道只服务扩展更新。
    assert_eq!(saved["update_public_key_set"], false);
    // 也不接受「随便写的源」：放行的是 scheme，不是格式。
    assert!(sandbox
        .refuses(&[
            "--set-update-source",
            "http://127.0.0.1:1/dev/",
            "--allow-insecure-source"
        ])
        .contains("origin"));

    let check = sandbox.run_json(&["--check-update"]);
    assert_eq!(check["update_source_set"], true);
    assert_eq!(check["update_source_insecure_allowed"], true);
    // 没有签名公钥时 Agent 自身的检查**根本不会发起**（所以连 update_error 都没有），
    // 扩展那一节试过但源不可达 —— 两者都不影响「源配好了」这件事。
    assert!(check.get("update_error").is_none(), "{check}");
    assert!(check.get("extension_latest_version").is_none(), "{check}");
    assert_eq!(check["update_available"], false);

    // 清空：入口没了，开关也一并归零。
    sandbox.run(&["--clear-update-source"]);
    let check = sandbox.run_json(&["--check-update"]);
    assert_eq!(check["update_source_set"], false);
    assert_eq!(check["update_source_insecure_allowed"], false);

    sandbox.cleanup();
}

#[test]
fn updates_are_refused_until_a_signed_channel_is_configured() {
    let sandbox = Sandbox::new();
    // No pinned key and no source: the agent must not even attempt a download.
    let check = sandbox.run_json(&["--check-update"]);
    assert_eq!(check["update_source_set"], false);
    assert_eq!(check["update_available"], false);

    let refusal = sandbox.refuses(&["--update"]);
    assert!(refusal.contains("签名更新源"), "{refusal}");
    let refusal = sandbox.refuses(&["--rollback"]);
    assert!(!refusal.is_empty());
}

#[test]
fn prune_keeps_the_active_version_and_never_fails_on_an_empty_install() {
    let sandbox = Sandbox::new();
    // Nothing installed yet: pruning must be a no-op rather than an error, and
    // must never be able to delete the running build.
    let removed = sandbox.run_json(&["--prune", "--host-name", DEV_HOST]);
    assert!(removed.is_array());
    assert!(removed.as_array().unwrap().is_empty());
}
