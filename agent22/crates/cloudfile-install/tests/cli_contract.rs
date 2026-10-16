//! 引导体的命令行契约。
//!
//! 它能验到「定源、失败报告、缺公钥时立刻停手」这几条 —— 也就是引导体**自己**的逻辑。
//! 「下载并交接给 native host」那一段需要一份签名清单，而签名要真通道的真私钥，
//! 所以那一步由 native host 侧的用例覆盖（同一个下载与验签实现）。
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::process::Command;

/// 最小 HTTP 服务：给每个请求回固定 body（引导体只判断「清单取回来了没有」）。
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

struct Sandbox {
    /// 只为了占住目录：TempDir 一 drop 就把安装目录删了。
    _directory: tempfile::TempDir,
    install_dir: PathBuf,
}

impl Sandbox {
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let install_dir = directory
            .path()
            .canonicalize()
            .unwrap()
            .join("CloudFileAgent22");
        Sandbox {
            _directory: directory,
            install_dir,
        }
    }

    fn command(&self) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_cloudfile-install"));
        command.env("CLOUDFILE_NO_PAUSE", "1");
        // 每个用例都在自己的目录里跑，绝不碰真实安装目录。
        command.env("CLOUDFILE_AGENT2_HOME", &self.install_dir);
        command.arg("--install-dir").arg(&self.install_dir);
        command
    }

    fn run(&self, arguments: &[&str]) -> (bool, String, String) {
        let output = self.command().args(arguments).output().unwrap();
        (
            output.status.success(),
            String::from_utf8_lossy(&output.stdout).into_owned(),
            String::from_utf8_lossy(&output.stderr).into_owned(),
        )
    }

    fn path(&self) -> &Path {
        &self.install_dir
    }
}

#[test]
fn help_documents_the_options_it_owns() {
    let sandbox = Sandbox::new();
    let (ok, stdout, _) = sandbox.run(&["--help"]);
    assert!(ok);
    for expected in [
        "--source",
        "--source-fallback",
        "--public-key",
        "--install-dir",
        "--dry-run",
    ] {
        assert!(stdout.contains(expected), "{expected} missing: {stdout}");
    }
    // 说明它会把其余参数转给 native host：这是它与 `--install` 的分工。
    assert!(
        stdout.contains("cloudfile-native-host --install"),
        "{stdout}"
    );
}

#[test]
fn dry_run_resolves_the_channel_and_changes_nothing() {
    let sandbox = Sandbox::new();
    let primary = serve(br#"{"version":"0.0.0"}"#);
    let (ok, stdout, stderr) = sandbox.run(&[
        "--dry-run",
        "--source",
        &primary,
        "--source-fallback",
        "http://127.0.0.1:1/mirror",
        "--allow-insecure-source",
    ]);
    assert!(ok, "{stderr}");
    let report: Value = serde_json::from_str(&stdout).expect("dry-run output is not JSON");
    assert_eq!(report["dry_run"], true);
    assert_eq!(report["update_source"], primary.as_str());
    assert_eq!(
        report["install_dir"].as_str().unwrap(),
        sandbox.path().to_string_lossy()
    );
    // 主源可用：备源一次都没被探。
    let probes = report["source_probes"].as_array().unwrap();
    assert_eq!(probes.len(), 1, "{report}");
    assert_eq!(probes[0]["error"], Value::Null);
    // dry-run 不改机器：连安装目录都不建。
    assert!(!sandbox.path().exists(), "dry-run 不该建目录");
}

#[test]
fn dry_run_reports_every_source_it_tried() {
    let sandbox = Sandbox::new();
    let (ok, _, stderr) = sandbox.run(&[
        "--dry-run",
        "--source",
        "http://127.0.0.1:1/primary",
        "--source-fallback",
        "http://127.0.0.1:2/mirror",
        "--allow-insecure-source",
    ]);
    assert!(!ok, "两个源都不通时必须失败");
    // 现场要看到试过谁、各自为什么不行。
    assert!(stderr.contains("http://127.0.0.1:1/primary"), "{stderr}");
    assert!(stderr.contains("http://127.0.0.1:2/mirror"), "{stderr}");
    assert!(stderr.contains("不可用"), "{stderr}");
}

#[test]
fn a_reachable_channel_without_a_pinned_key_stops_before_downloading() {
    let sandbox = Sandbox::new();
    let source = serve(br#"{"version":"0.0.0"}"#);
    let (ok, _, stderr) = sandbox.run(&[
        "--source",
        &source,
        "--allow-insecure-source",
        "--extension-id",
        "abcdefghijklmnopabcdefghijklmnop",
    ]);
    assert!(!ok);
    // 信任锚不能从通道拿：没内置、也没显式给，就必须停手并说清楚怎么给。
    assert!(stderr.contains("--public-key"), "{stderr}");
    assert!(stderr.contains("CLOUDFILE_INSTALL_PUBLIC_KEY"), "{stderr}");
    // 停在下载之前：安装目录里不该出现任何东西（连 host 副本都不下载）。
    let entries: Vec<String> = std::fs::read_dir(sandbox.path())
        .map(|entries| {
            entries
                .flatten()
                .map(|entry| entry.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default();
    assert!(entries.is_empty(), "缺公钥时不该下载任何文件：{entries:?}");
}

#[test]
fn an_invalid_pinned_key_is_refused_before_downloading() {
    let sandbox = Sandbox::new();
    let source = serve(br#"{"version":"0.0.0"}"#);
    let (ok, _, stderr) = sandbox.run(&[
        "--source",
        &source,
        "--allow-insecure-source",
        "--public-key",
        "not-a-key",
        "--extension-id",
        "abcdefghijklmnopabcdefghijklmnop",
    ]);
    assert!(!ok);
    assert!(stderr.contains("公钥不可用"), "{stderr}");
}

#[test]
fn a_bare_flag_is_forwarded_with_its_value() {
    let sandbox = Sandbox::new();
    let (ok, stdout, stderr) = sandbox.run(&[
        "--dry-run",
        "--source",
        "http://127.0.0.1:1/primary",
        "--source-fallback",
        "http://127.0.0.1:2/mirror",
        "--allow-insecure-source",
        "--extension-id",
        "abcdefghijklmnopabcdefghijklmnop",
        "--skip-autostart",
    ]);
    // 源都不通，所以只看转发：失败信息里不该出现「无法识别的参数」。
    assert!(!ok);
    assert!(!stderr.contains("无法识别"), "{stderr}");
    assert!(stdout.is_empty(), "失败时 stdout 不该有报告：{stdout}");
}
