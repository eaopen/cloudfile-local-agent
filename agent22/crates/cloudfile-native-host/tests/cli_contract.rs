//! End-to-end contract for the shipped installer CLI.
//!
//! Registration, autostart and revert are the parts of the 2.2 plan that unit
//! tests cannot prove: they depend on the real executable, the real per-user
//! directories and the platform's host manifest layout. Every case below runs
//! the actual binary with `HOME` (and on Windows `LOCALAPPDATA`) redirected to
//! a temporary directory, so the developer's own Chrome registration and private
//! store are never touched.
use serde_json::Value;
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
    assert!(sandbox
        .refuses(&["--register-host", "--host-name", DEV_HOST])
        .contains("--extension-id is required"));
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
    assert_eq!(status["autostart"], false);
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
