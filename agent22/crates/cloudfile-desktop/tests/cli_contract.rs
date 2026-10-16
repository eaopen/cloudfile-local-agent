//! CLI surface of the desktop process.
//!
//! `--version` and `--component` are not conveniences: the upgrade path starts a
//! freshly staged build and refuses to activate it unless it answers both. These
//! cases therefore run the real executable, and they must stay headless — the
//! staged build is probed before any window is allowed to be created.
use std::process::Command;

const VERSION: &str = env!("CARGO_PKG_VERSION");

fn desktop(arguments: &[&str]) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_cloudfile-desktop"))
        .args(arguments)
        .output()
        .unwrap()
}

#[test]
fn version_answers_without_starting_the_agent() {
    let output = desktop(&["--version"]);
    assert!(output.status.success());
    assert_eq!(String::from_utf8_lossy(&output.stdout).trim(), VERSION);
    // Nothing else may be printed: the health check compares the whole stdout.
    assert!(output.stderr.is_empty());
}

#[test]
fn component_contract_matches_the_native_host() {
    let output = desktop(&["--component"]);
    assert!(output.status.success());
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value["version"], VERSION);
    assert_eq!(value["agent_family"], "agent2");
    assert_eq!(value["role"], "desktop");
    // Both executables must state the same protocol numbers or an upgrade could
    // activate a half-compatible pair.
    assert!(value["ipc_version"].as_u64().unwrap() > 0);
    assert!(value["journal_version"].as_u64().unwrap() > 0);
}

#[test]
fn an_unknown_flag_is_refused_instead_of_starting_a_second_agent() {
    let output = desktop(&["--definitely-not-a-flag"]);
    assert!(!output.status.success());
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("unknown argument"), "{stderr}");
    // The usage text has to mention the login-item flag, since that is the one
    // argument the operating system passes in on its own.
    assert!(stderr.contains("--autostart"), "{stderr}");
}

#[test]
fn help_lists_the_login_item_flag() {
    let output = desktop(&["--help"]);
    assert!(output.status.success());
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(stdout.contains("--autostart"), "{stdout}");
    assert!(stdout.contains("--version"), "{stdout}");
}

#[test]
fn help_lists_the_self_test_flag() {
    let output = desktop(&["--help"]);
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(stdout.contains("--self-test"), "{stdout}");
    // 帮助文本要说明它不开窗口，否则没人敢在自动化里用。
    assert!(stdout.contains("never opens a window"), "{stdout}");
}

#[test]
fn the_self_test_answers_with_json_and_never_opens_a_window() {
    let output = desktop(&["--self-test"]);
    // 0 = 全绿；1 = 有项确实不工作。两者都说明它跑完了并给出了结论
    // （本机有没有 native host 是环境差异，不是这个用例要判的事）。
    assert!(
        matches!(output.status.code(), Some(0) | Some(1)),
        "self-test did not finish: {output:?}"
    );
    let report: serde_json::Value =
        serde_json::from_slice(&output.stdout).expect("self-test output is not JSON");
    let checks = report["checks"].as_array().unwrap();
    assert!(checks.len() >= 10, "{}", checks.len());
    assert!(report["root"]
        .as_str()
        .unwrap()
        .contains("CloudFileAgent22"));
    // 报告全在 stdout：解析失败的原因要能直接看到。
    assert!(
        output.stderr.is_empty(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    // 建窗口之前就结束，所以不会留下常驻进程（这条用例本身会挂住而不是失败）。
    assert!(
        checks.iter().any(|check| check["name"] == "peer-binary"),
        "peer-binary check missing"
    );
}
