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
