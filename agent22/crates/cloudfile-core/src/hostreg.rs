//! Native messaging host registration owned by the agent itself.
//!
//! The historical installer was a Python or PowerShell script, which meant the
//! user's machine needed a scripting runtime just to connect the extension. The
//! agent now performs every registration step itself, keeps a rollback copy of
//! whatever it replaced, and refuses to overwrite a host owned by another
//! program.
//!
//! Development and production identities stay separate on purpose: the dev
//! build registers `com.cloudfile.agent22.dev` and never touches the hosts the
//! shipped Windows and macOS agents use.
use crate::storage;
use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::fs;
use std::path::{Path, PathBuf};

/// Isolated development host. Registered by this agent only.
pub const DEV_HOST: &str = "com.cloudfile.agent22.dev";
/// Hosts the shipped agents own. Listed so takeover can be explicit and
/// reversible; never rewritten during development.
pub const LEGACY_HOSTS: &[&str] = &[
    "com.cloudfile.agent2",
    "com.cloudfile.current_agent",
    "com.cloudfile.local_agent",
];

pub const DESCRIPTION: &str = "CloudFile Agent 2.2 Native Host";

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Registration {
    pub host: String,
    pub manifest_path: PathBuf,
    pub binary: Option<PathBuf>,
    pub extension_id: Option<String>,
    /// A different program currently owns this host name.
    pub foreign: bool,
}

pub fn validate_extension_id(value: &str) -> Result<()> {
    if value.len() != 32 || !value.bytes().all(|b| (b'a'..=b'p').contains(&b)) {
        bail!("invalid Chrome extension ID");
    }
    Ok(())
}

pub fn validate_host(value: &str) -> Result<()> {
    let ok = value.len() <= 128
        && value.contains('.')
        && value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '.' || c == '_' || c == '-');
    if !ok {
        bail!("invalid native messaging host name");
    }
    Ok(())
}

/// The exact manifest Chrome requires. Field order is fixed so a takeover diff
/// stays readable.
pub fn manifest(name: &str, binary: &Path, extension_id: &str) -> Result<Value> {
    validate_host(name)?;
    validate_extension_id(extension_id)?;
    if !binary.is_absolute() {
        bail!("native host binary path must be absolute");
    }
    Ok(json!({
        "name": name,
        "description": DESCRIPTION,
        "path": binary.to_string_lossy(),
        "type": "stdio",
        "allowed_origins": [format!("chrome-extension://{extension_id}/")],
    }))
}

/// Chrome looks for the manifest in a fixed per-platform directory.
pub fn default_manifest_dir(
    os: &str,
    home: &Path,
    local_app_data: Option<&Path>,
) -> Result<PathBuf> {
    match os {
        "macos" => Ok(home.join("Library/Application Support/Google/Chrome/NativeMessagingHosts")),
        "windows" => {
            let local = local_app_data.context("LOCALAPPDATA is unavailable")?;
            Ok(local.join("CloudFileAgent22").join("native-host"))
        }
        _ => bail!("native host registration supports Windows and macOS only"),
    }
}

pub fn manifest_dir() -> Result<PathBuf> {
    let home = dirs::home_dir().context("home directory unavailable")?;
    let local = std::env::var_os("LOCALAPPDATA").map(PathBuf::from);
    default_manifest_dir(std::env::consts::OS, &home, local.as_deref())
}

pub fn manifest_path(dir: &Path, host: &str) -> PathBuf {
    dir.join(format!("{host}.json"))
}

/// Where the replaced manifest is kept so a bad upgrade can be undone offline.
pub fn previous_manifest_path(dir: &Path, host: &str) -> PathBuf {
    dir.join(format!("{host}.previous.json"))
}

/// 当前用户的 Chrome NativeMessagingHosts 键（带 hive 前缀的完整路径）。
/// 纯函数，所以「查存在」和「写/删」用的一定是同一个键，不会漂移。
pub fn registry_key(host: &str) -> String {
    format!("HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\{host}")
}

/// 这次注册要在注册表里落下的终态：`Some(path)` = 默认值写成 manifest 路径，
/// `None` = 整个键删掉。拆成纯函数是为了让「写什么」在跨平台测试里被钉住，
/// 真正的落盘由 [`winreg`]（Windows，进程内）完成。
pub fn registry_update(host: &str, manifest: Option<&Path>) -> Result<(String, Option<String>)> {
    validate_host(host)?;
    let key = registry_key(host);
    match manifest {
        Some(path) => {
            if !path.is_absolute() {
                bail!("native host manifest path must be absolute");
            }
            Ok((key, Some(path.to_string_lossy().into_owned())))
        }
        None => Ok((key, None)),
    }
}

#[cfg(windows)]
fn apply_registry(host: &str, manifest: Option<&Path>) -> Result<()> {
    // 进程内读写：以前每注册一次要起两个 `reg.exe`（先 query 探存在、再 add/delete），
    // 在 GUI 父进程下就是两次黑窗口，且机器禁用 reg.exe 时直接失败。
    let (key, value) = registry_update(host, manifest)?;
    match value {
        Some(path) => crate::winreg::write_string(&key, "", &path),
        // 删掉整个键（含子键）。键本来就不在也算成功：「删完不存在」是调用方要的终态。
        None => crate::winreg::delete_key(&key),
    }
}

#[cfg(not(windows))]
fn apply_registry(host: &str, manifest: Option<&Path>) -> Result<()> {
    // Unix 侧注册就是固定目录里的一个文件，没有注册表可写；
    // 这里只保留 host/manifest 的校验（与 Windows 口径一致），然后什么都不做。
    registry_update(host, manifest).map(|_| ())
}

pub fn state(dir: &Path, host: &str, expected_binary: Option<&Path>) -> Result<Registration> {
    validate_host(host)?;
    let path = manifest_path(dir, host);
    let mut registration = Registration {
        host: host.to_string(),
        manifest_path: path.clone(),
        binary: None,
        extension_id: None,
        foreign: false,
    };
    if !path.is_file() {
        return Ok(registration);
    }
    let value: Value = storage::read(&path)?;
    let binary = value["path"].as_str().map(PathBuf::from);
    registration.extension_id = value["allowed_origins"]
        .as_array()
        .and_then(|origins| origins.first())
        .and_then(|origin| origin.as_str())
        .and_then(|origin| origin.strip_prefix("chrome-extension://"))
        .map(|origin| origin.trim_end_matches('/').to_string());
    registration.foreign = match (expected_binary, &binary) {
        (Some(expected), Some(found)) => found != expected,
        _ => false,
    };
    registration.binary = binary;
    Ok(registration)
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct InstallOutcome {
    pub host: String,
    pub manifest_path: PathBuf,
    pub binary: PathBuf,
    /// A previous manifest was preserved at `previous_manifest_path`.
    pub backed_up: bool,
    pub previous_manifest_path: PathBuf,
}

/// Registers exactly one host for the current user.
///
/// `takeover` is required to replace a host that another program currently
/// owns; the previous manifest is always preserved first.
pub fn install(
    dir: &Path,
    host: &str,
    extension_id: &str,
    binary: &Path,
    takeover: bool,
) -> Result<InstallOutcome> {
    validate_extension_id(extension_id)?;
    validate_host(host)?;
    let binary = binary
        .canonicalize()
        .context("native host binary is missing")?;
    if !binary.is_file() {
        bail!("native host binary is missing");
    }
    if cfg!(windows) && binary.extension().is_none() {
        bail!("native host binary must keep its executable extension");
    }
    storage::private_dir(dir)?;
    let path = manifest_path(dir, host);
    let previous = previous_manifest_path(dir, host);
    let mut backed_up = false;
    if path.exists() {
        let current: Value = storage::read(&path)?;
        let replaced = manifest(host, &binary, extension_id)?;
        if current != replaced {
            let owner = current["path"].as_str().unwrap_or_default();
            if !takeover && !is_agent_binary(Path::new(owner)) {
                bail!("{host} is registered by another program; run takeover to replace it");
            }
            fs::rename(&path, &previous)?;
            backed_up = true;
        }
    }
    let bytes = serde_json::to_vec_pretty(&manifest(host, &binary, extension_id)?)?;
    storage::write_bytes(&path, &bytes)?;
    // A failed registry write leaves the manifest in place but the old key
    // pointing elsewhere; that is the safe direction and is reported.
    if cfg!(windows) {
        apply_registry(host, Some(&path))?;
    }
    Ok(InstallOutcome {
        host: host.to_string(),
        manifest_path: path,
        binary,
        backed_up,
        previous_manifest_path: previous,
    })
}

fn is_agent_binary(path: &Path) -> bool {
    path.file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| name.starts_with("cloudfile-native-host"))
}

/// Removes only a manifest this agent owns. A foreign host is left untouched.
pub fn unregister(dir: &Path, host: &str) -> Result<PathBuf> {
    validate_host(host)?;
    let path = manifest_path(dir, host);
    if !path.exists() {
        if cfg!(windows) {
            apply_registry(host, None)?;
        }
        return Ok(path);
    }
    let value: Value = storage::read(&path)?;
    let owned = value["description"]
        .as_str()
        .is_some_and(|text| text.starts_with("CloudFile Agent"))
        && value["type"].as_str() == Some("stdio");
    if !owned {
        bail!("{host} was not registered by this agent; refusing to remove it");
    }
    fs::remove_file(&path)?;
    if cfg!(windows) {
        apply_registry(host, None)?;
    }
    Ok(path)
}

/// Restores the manifest that was replaced by the last successful install.
pub fn rollback(dir: &Path, host: &str) -> Result<PathBuf> {
    validate_host(host)?;
    let previous = previous_manifest_path(dir, host);
    if !previous.is_file() {
        bail!("no previous native host registration to restore");
    }
    let value: Value = storage::read(&previous)?;
    if value["name"].as_str() != Some(host) || value["type"].as_str() != Some("stdio") {
        bail!("previous registration is not a usable native host manifest");
    }
    let path = manifest_path(dir, host);
    let bytes = serde_json::to_vec_pretty(&value)?;
    storage::write_bytes(&path, &bytes)?;
    if cfg!(windows) {
        apply_registry(host, Some(&path))?;
    }
    Ok(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "abcdefghijklmnopabcdefghijklmnop";

    /// An absolute directory for the host OS. `Path::is_absolute` is false for
    /// `/cloudfile` on Windows, and `registry_update` refuses a manifest
    /// path that is not absolute, so a Unix literal would fail there.
    #[cfg(windows)]
    const BASE: &str = "C:/cloudfile";
    #[cfg(not(windows))]
    const BASE: &str = "/cloudfile";

    fn binary(dir: &Path) -> PathBuf {
        let name = if cfg!(windows) {
            "cloudfile-native-host.exe"
        } else {
            "cloudfile-native-host"
        };
        let path = dir.join(name);
        fs::write(&path, b"bin").unwrap();
        path.canonicalize().unwrap()
    }

    #[test]
    fn manifest_matches_the_chrome_contract() {
        let (_dir, base) = crate::storage::test_root();
        let path = binary(&base);
        let value = manifest(DEV_HOST, &path, ID).unwrap();
        assert_eq!(value["name"], DEV_HOST);
        assert_eq!(value["type"], "stdio");
        assert_eq!(value["path"], path.to_string_lossy().as_ref());
        assert_eq!(
            value["allowed_origins"][0],
            format!("chrome-extension://{ID}/")
        );
        assert!(manifest(DEV_HOST, &path, "not-an-id").is_err());
        assert!(manifest("bad host", &path, ID).is_err());
        assert!(manifest(DEV_HOST, Path::new("relative"), ID).is_err());
    }

    #[test]
    fn manifest_dir_follows_the_platform_contract() {
        let home = Path::new("/home/user");
        let local = Path::new("C:/Users/user/AppData/Local");
        assert_eq!(
            default_manifest_dir("macos", home, None).unwrap(),
            home.join("Library/Application Support/Google/Chrome/NativeMessagingHosts")
        );
        assert_eq!(
            default_manifest_dir("windows", home, Some(local)).unwrap(),
            local.join("CloudFileAgent22/native-host")
        );
        assert!(default_manifest_dir("linux", home, Some(local)).is_err());
        assert!(default_manifest_dir("windows", home, None).is_err());
    }

    #[test]
    fn the_registry_key_is_fixed_for_the_current_user() {
        assert_eq!(
            registry_key(DEV_HOST),
            "HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\com.cloudfile.agent22.dev"
        );
        assert_eq!(
            registry_key(LEGACY_HOSTS[0]),
            "HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\com.cloudfile.agent2"
        );
    }

    #[test]
    fn registry_update_is_exact_and_reversible() {
        let manifest = Path::new(BASE).join("native-host/com.cloudfile.agent22.dev.json");
        // 写：键 + 要落到默认值里的 manifest 绝对路径。
        let (key, value) = registry_update(DEV_HOST, Some(&manifest)).unwrap();
        assert_eq!(
            key,
            "HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\com.cloudfile.agent22.dev"
        );
        assert_eq!(value.as_deref(), Some(manifest.to_string_lossy().as_ref()));
        // 删：同一个键，值为 None —— 整个键摘掉，Chrome 就再也找不到这个 Host。
        let (key, value) = registry_update(DEV_HOST, None).unwrap();
        assert_eq!(
            key,
            "HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\com.cloudfile.agent22.dev"
        );
        assert_eq!(value, None);
        assert!(registry_update(DEV_HOST, Some(Path::new("relative.json"))).is_err());
    }

    #[test]
    fn install_is_reversible_and_guards_foreign_hosts() {
        let (_dir, base) = crate::storage::test_root();
        let manifest_dir = base.join("hosts");
        let path = binary(&base);
        let first = install(&manifest_dir, DEV_HOST, ID, &path, false).unwrap();
        assert!(!first.backed_up);
        assert!(first.manifest_path.is_file());
        assert_eq!(
            state(&manifest_dir, DEV_HOST, Some(&path))
                .unwrap()
                .extension_id
                .as_deref(),
            Some(ID)
        );

        // A foreign owner cannot be replaced silently.
        storage::write(
            &manifest_path(&manifest_dir, DEV_HOST),
            &json!({"name":DEV_HOST,"type":"stdio","path":"/opt/other/host"}),
        )
        .unwrap();
        assert!(install(&manifest_dir, DEV_HOST, ID, &path, false).is_err());
        assert!(state(&manifest_dir, DEV_HOST, Some(&path)).unwrap().foreign);

        let second = install(&manifest_dir, DEV_HOST, ID, &path, true).unwrap();
        assert!(second.backed_up);
        assert!(second.previous_manifest_path.is_file());
        rollback(&manifest_dir, DEV_HOST).unwrap();
        let restored: Value = storage::read(&manifest_path(&manifest_dir, DEV_HOST)).unwrap();
        assert_eq!(restored["path"], "/opt/other/host");
        // The restored manifest belongs to another program and stays untouched.
        assert!(unregister(&manifest_dir, DEV_HOST).is_err());
        install(&manifest_dir, DEV_HOST, ID, &path, true).unwrap();
        unregister(&manifest_dir, DEV_HOST).unwrap();
        assert!(!manifest_path(&manifest_dir, DEV_HOST).exists());
    }

    #[test]
    fn unregister_refuses_a_manifest_this_agent_did_not_write() {
        let (_dir, base) = crate::storage::test_root();
        storage::write(
            &manifest_path(&base, LEGACY_HOSTS[0]),
            &json!({"name":LEGACY_HOSTS[0],"type":"stdio","path":"/opt/legacy/host","description":"Legacy"}),
        )
        .unwrap();
        assert!(unregister(&base, LEGACY_HOSTS[0]).is_err());
        assert!(manifest_path(&base, LEGACY_HOSTS[0]).exists());
        // Absent hosts are a no-op, not an error.
        assert!(unregister(&base, DEV_HOST).is_ok());
        assert!(rollback(&base, DEV_HOST).is_err());
    }
}
