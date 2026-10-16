//! Versioned installation and revertible upgrade for the desktop agent.
//!
//! Every release installs into its own `bin/<version>` directory, so a running
//! agent is never overwritten. Activation is a single atomic rewrite of the
//! native host manifest `path` field; the replaced manifest is preserved first,
//! which makes `rollback` an offline operation.
//!
//! Trust model: the signed release manifest is the root of trust. The signature
//! covers the version, platform, protocol versions, expiry and every artifact
//! digest, so an unsigned or partially modified manifest is rejected outright.
//! There is deliberately no "unsigned local build" escape hatch: a manual
//! upgrade supplies the same signed manifest through `--release`.
use crate::{hostreg, storage};
use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use p256::ecdsa::{signature::Verifier, Signature, VerifyingKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::cmp::Ordering;
use std::fs;
use std::path::{Path, PathBuf};

/// Local journal/session protocol versions this build can read.
pub const IPC_VERSION: u32 = 1;
pub const JOURNAL_VERSION: u32 = 1;

const MAX_RELEASE_BYTES: usize = 64 * 1024;
const DOMAIN: &str = "cloudfile.agent2.release.v1";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Binary {
    NativeHost,
    Desktop,
}

impl Binary {
    pub fn kind(self) -> &'static str {
        match self {
            Binary::NativeHost => "native-host",
            Binary::Desktop => "desktop",
        }
    }
    fn parse(value: &str) -> Result<Self> {
        match value {
            "native-host" => Ok(Binary::NativeHost),
            "desktop" => Ok(Binary::Desktop),
            _ => bail!("unknown release artifact kind"),
        }
    }
    pub fn file_name(self, os: &str, version: &str) -> String {
        let base = match self {
            Binary::NativeHost => format!("cloudfile-native-host-{version}"),
            Binary::Desktop => format!("cloudfile-desktop-{version}"),
        };
        if os == "windows" {
            format!("{base}.exe")
        } else {
            base
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Artifact {
    pub kind: String,
    pub url: String,
    pub sha256: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Release {
    pub version: String,
    pub platform: String,
    pub notes: String,
    pub expires_at: u64,
    pub ipc_version: u32,
    pub journal_version: u32,
    pub artifacts: Vec<Artifact>,
    pub signature: String,
}

pub fn platform_id(os: &str, arch: &str) -> String {
    format!("{os}-{arch}")
}

/// Everything the signature covers, in a fixed order. Reproducing this string
/// is the whole contract for a release signer.
pub fn signing_payload(release: &Release) -> Vec<u8> {
    let mut payload = String::new();
    payload.push_str(DOMAIN);
    payload.push('\n');
    payload.push_str(&release.version);
    payload.push('\n');
    payload.push_str(&release.platform);
    payload.push('\n');
    payload.push_str(&release.notes);
    payload.push('\n');
    payload.push_str(&release.expires_at.to_string());
    payload.push('\n');
    payload.push_str(&release.ipc_version.to_string());
    payload.push('\n');
    payload.push_str(&release.journal_version.to_string());
    payload.push('\n');
    for artifact in &release.artifacts {
        payload.push_str(&format!(
            "{}\t{}\t{}\n",
            artifact.kind, artifact.url, artifact.sha256
        ));
    }
    payload.into_bytes()
}

pub(crate) fn valid_version(value: &str) -> bool {
    if value.is_empty() || value.len() > 32 {
        return false;
    }
    let (core, suffix) = match value.split_once('-') {
        Some((core, suffix)) => (core, Some(suffix)),
        None => (value, None),
    };
    let parts: Vec<&str> = core.split('.').collect();
    if parts.len() != 3
        || parts
            .iter()
            .any(|part| part.is_empty() || !part.bytes().all(|b| b.is_ascii_digit()))
    {
        return false;
    }
    match suffix {
        None => true,
        Some(suffix) => {
            !suffix.is_empty()
                && suffix.len() <= 16
                && suffix
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '.' || c == '-')
        }
    }
}

/// Orders dotted versions by numeric weight. A pre-release sorts before its
/// release, matching the usual expectation that `2.2.0-rc.1 < 2.2.0`.
pub fn compare(left: &str, right: &str) -> Ordering {
    let split = |value: &str| -> (Vec<u64>, Option<String>) {
        let (core, suffix) = match value.split_once('-') {
            Some((core, suffix)) => (core, Some(suffix.to_string())),
            None => (value, None),
        };
        (
            core.split('.')
                .map(|part| part.parse::<u64>().unwrap_or(0))
                .collect(),
            suffix,
        )
    };
    let (left_core, left_suffix) = split(left);
    let (right_core, right_suffix) = split(right);
    for index in 0..left_core.len().max(right_core.len()) {
        let a = left_core.get(index).copied().unwrap_or(0);
        let b = right_core.get(index).copied().unwrap_or(0);
        if a != b {
            return a.cmp(&b);
        }
    }
    match (left_suffix, right_suffix) {
        (None, None) => Ordering::Equal,
        (None, Some(_)) => Ordering::Greater,
        (Some(_), None) => Ordering::Less,
        (Some(a), Some(b)) => a.cmp(&b),
    }
}

pub fn valid_sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// 更新源的信任策略。
///
/// 正式通道只认**规范 https origin**（`https://host/dir`：不带尾斜杠、不带用户信息、
/// 不带 fragment）。更新包是按这个 origin 解析并下载的，所以「源写成什么样」和
/// 「允许下载到哪」必须是同一条规则，不能靠调用方各自解释。
///
/// 开发通道是局域网上的**明文 http**（例如 `http://10.9.8.162:6111/cloudfile-updates-dev`）。
/// 它必须被**显式**打开：`allow_insecure` 只能来自 config.json 的
/// `allow_insecure_update_source` 或命令行的 `--allow-insecure-source`，
/// **绝不会因为源本身写成 http 就自动放行**。打开时也只放宽 scheme 一项 ——
/// 规范 origin、同源、sha256、签名校验一条都不放松。
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct SourcePolicy {
    /// 允许明文 http 源（仅开发通道；正式通道永远是 https）。
    pub allow_insecure: bool,
}

impl SourcePolicy {
    /// 正式通道：只认 https。
    pub const STRICT: SourcePolicy = SourcePolicy {
        allow_insecure: false,
    };
    /// 开发通道：额外允许明文 http，其余规则不变。
    pub const DEV: SourcePolicy = SourcePolicy {
        allow_insecure: true,
    };

    fn permits(self, scheme: &str) -> bool {
        scheme == "https" || (self.allow_insecure && scheme == "http")
    }

    /// 源与目标是否同一个 origin（并且是允许的 scheme）。
    pub fn same_origin(self, source: &str, url: &str) -> bool {
        let (Ok(source), Ok(url)) = (url::Url::parse(source), url::Url::parse(url)) else {
            return false;
        };
        self.permits(source.scheme())
            && self.permits(url.scheme())
            && source.origin() == url.origin()
            && url.username().is_empty()
            && url.password().is_none()
            && url.fragment().is_none()
    }

    /// 更新源本身必须是规范 origin（严格到「重新序列化后一字不差」）。
    pub fn valid_source(self, source: &str) -> bool {
        url::Url::parse(source)
            .map(|url| url.as_str().trim_end_matches('/') == source)
            .unwrap_or(false)
            && self.same_origin(source, source)
    }
}

/// 正式通道下的源校验（等价于 [`SourcePolicy::STRICT`]）。
pub fn valid_source(source: &str) -> bool {
    SourcePolicy::STRICT.valid_source(source)
}

/// Decodes the pinned public key: base64url of the 65-byte uncompressed point.
pub fn verifying_key(encoded: &str) -> Result<VerifyingKey> {
    let bytes = B64
        .decode(encoded)
        .context("release public key is not base64url")?;
    if bytes.len() != 65 || bytes[0] != 4 {
        bail!("release public key must be an uncompressed P-256 point");
    }
    let point = p256::EncodedPoint::from_bytes(bytes).context("invalid P-256 point")?;
    VerifyingKey::from_encoded_point(&point).context("invalid P-256 public key")
}

/// Full verification. Nothing is downloaded before every field below passes.
pub fn verify(
    policy: SourcePolicy,
    release: &Release,
    source: &str,
    current_version: &str,
    now: u64,
    platform: &str,
    public_key: &str,
) -> Result<()> {
    if !valid_version(&release.version) {
        bail!("release version is malformed");
    }
    if release.platform != platform {
        bail!("release is built for a different platform");
    }
    if compare(&release.version, current_version) != Ordering::Greater {
        bail!("release is not newer than the installed version");
    }
    if release.expires_at <= now {
        bail!("release manifest has expired");
    }
    if release.ipc_version != IPC_VERSION || release.journal_version != JOURNAL_VERSION {
        bail!("release speaks an incompatible protocol version");
    }
    if !policy.valid_source(source) {
        bail!(
            "update source must be a canonical {} origin",
            if policy.allow_insecure {
                "https or http"
            } else {
                "https"
            }
        );
    }
    let mut seen = std::collections::BTreeSet::new();
    for artifact in &release.artifacts {
        Binary::parse(&artifact.kind)?;
        if !seen.insert(artifact.kind.clone()) {
            bail!("release lists the same artifact twice");
        }
        if !valid_sha256(&artifact.sha256) {
            bail!("release artifact is missing a usable digest");
        }
        if !policy.same_origin(source, &artifact.url) {
            bail!("release artifact does not come from the update source");
        }
    }
    if seen.len() != 2 || !seen.contains("native-host") || !seen.contains("desktop") {
        bail!("release must contain both agent binaries");
    }
    let signature = B64
        .decode(&release.signature)
        .context("release signature is not base64url")?;
    let signature = Signature::from_slice(&signature).context("invalid release signature")?;
    verifying_key(public_key)?
        .verify(&signing_payload(release), &signature)
        .context("release signature does not match the pinned key")?;
    Ok(())
}

pub fn parse_release(bytes: &[u8]) -> Result<Release> {
    if bytes.len() > MAX_RELEASE_BYTES {
        bail!("release manifest is too large");
    }
    Ok(serde_json::from_slice(bytes)?)
}

/// Fixed name under the update origin. No redirects: a re-pointed manifest
/// would move the trust anchor away from the pinned key.
pub fn fetch_manifest(source: &str) -> Result<Vec<u8>> {
    fetch_manifest_for(SourcePolicy::STRICT, source)
}

/// 同上，但按给定策略判定 scheme（开发通道放行明文 http）。
pub fn fetch_manifest_for(policy: SourcePolicy, source: &str) -> Result<Vec<u8>> {
    if !policy.valid_source(source) {
        bail!(
            "update source must be a canonical {} origin",
            if policy.allow_insecure {
                "https or http"
            } else {
                "https"
            }
        );
    }
    let client = reqwest::blocking::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(std::time::Duration::from_secs(30))
        .build()?;
    let response = client
        .get(format!("{source}/cloudfile-agent2-update.json"))
        .send()
        .context("update manifest request failed")?;
    if response.status() != 200 {
        bail!("update manifest rejected");
    }
    let mut bytes = Vec::new();
    use std::io::Read;
    response
        .take(MAX_RELEASE_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > MAX_RELEASE_BYTES {
        bail!("release manifest is too large");
    }
    Ok(bytes)
}

/// Downloads one artifact and checks its digest before it is written anywhere.
pub fn download(artifact: &Artifact) -> Result<Vec<u8>> {
    let client = reqwest::blocking::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(std::time::Duration::from_secs(300))
        .build()?;
    let response = client
        .get(&artifact.url)
        .send()
        .context("download failed")?;
    if response.status() != 200 {
        bail!("download rejected");
    }
    let limit = 64 * 1024 * 1024u64;
    let mut bytes = Vec::new();
    use std::io::Read;
    response.take(limit + 1).read_to_end(&mut bytes)?;
    if bytes.len() as u64 > limit {
        bail!("artifact is larger than the supported limit");
    }
    let digest = format!("{:x}", Sha256::digest(&bytes));
    if digest != artifact.sha256 {
        bail!("artifact digest does not match the signed manifest");
    }
    Ok(bytes)
}

pub fn install_root(exe: &Path) -> PathBuf {
    let directory = exe.parent().unwrap_or(exe);
    for ancestor in directory.ancestors() {
        if ancestor.file_name().is_some_and(|name| name == "bin") {
            return ancestor
                .parent()
                .map(Path::to_path_buf)
                .unwrap_or_else(|| directory.to_path_buf());
        }
    }
    directory.to_path_buf()
}

pub fn bin_dir(root: &Path) -> PathBuf {
    root.join("bin")
}

pub fn version_dir(root: &Path, version: &str) -> Result<PathBuf> {
    if !valid_version(version) {
        bail!("invalid version directory");
    }
    Ok(bin_dir(root).join(version))
}

pub fn installed_versions(root: &Path) -> Result<Vec<String>> {
    let dir = bin_dir(root);
    let mut versions = Vec::new();
    if !dir.exists() {
        return Ok(versions);
    }
    for entry in fs::read_dir(&dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        if valid_version(&name) {
            versions.push(name);
        }
    }
    versions.sort_by(|a, b| compare(a, b));
    Ok(versions)
}

/// The active version is derived from the manifest, never from a second file
/// that could drift out of sync with what Chrome actually launches.
pub fn active_version(dir: &Path, host: &str) -> Result<Option<String>> {
    let path = hostreg::manifest_path(dir, host);
    if !path.is_file() {
        return Ok(None);
    }
    let value: serde_json::Value = storage::read(&path)?;
    let Some(binary) = value["path"].as_str() else {
        return Ok(None);
    };
    Ok(Path::new(binary)
        .parent()
        .and_then(Path::file_name)
        .and_then(|name| name.to_str())
        .filter(|name| valid_version(name))
        .map(str::to_string))
}

/// Writes a staging directory next to the final one, then renames it into
/// place. A crash leaves either the old state or a `.staging` leftover, never
/// a half installed version that the manifest could point at.
pub fn stage(root: &Path, version: &str, os: &str, files: &[(Binary, Vec<u8>)]) -> Result<PathBuf> {
    if !valid_version(version) {
        bail!("invalid version directory");
    }
    let target = version_dir(root, version)?;
    if target.exists() {
        bail!("that version is already installed");
    }
    let staging = bin_dir(root).join(format!(".staging-{version}-{}", uuid::Uuid::new_v4()));
    storage::private_dir(&staging)?;
    for (kind, bytes) in files {
        if bytes.is_empty() {
            bail!("refusing to stage an empty binary");
        }
        let path = staging.join(kind.file_name(os, version));
        storage::write_bytes(&path, bytes)?;
        set_executable(&path)?;
    }
    fs::rename(&staging, &target)?;
    Ok(target)
}

fn set_executable(path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o755))?;
    }
    #[cfg(not(unix))]
    let _ = path;
    Ok(())
}

/// Points the manifest at a staged version. The previous manifest is preserved
/// by `hostreg::install`, which also updates the Windows registry key.
pub fn activate(root: &Path, dir: &Path, host: &str, version: &str) -> Result<PathBuf> {
    let directory = version_dir(root, version)?;
    let os = std::env::consts::OS;
    let host_binary = directory.join(Binary::NativeHost.file_name(os, version));
    let desktop_binary = directory.join(Binary::Desktop.file_name(os, version));
    if !host_binary.is_file() || !desktop_binary.is_file() {
        bail!("staged version is incomplete; activation refused");
    }
    let current = hostreg::state(dir, host, None)?;
    let extension_id = current
        .extension_id
        .clone()
        .context("no existing registration to take over; register the host first")?;
    hostreg::install(dir, host, &extension_id, &host_binary, true)?;
    Ok(host_binary)
}

/// Restores the manifest that preceded the last activation.
pub fn rollback(dir: &Path, host: &str) -> Result<PathBuf> {
    hostreg::rollback(dir, host)
}

/// Keeps the newest versions for offline revert and removes the rest. The
/// active version is always kept even if it is not the newest one.
pub fn prune(root: &Path, host: &str, dir: &Path, keep: usize) -> Result<Vec<String>> {
    let keep = keep.max(1);
    let active = active_version(dir, host).unwrap_or(None);
    let mut versions = installed_versions(root)?;
    let mut removed = Vec::new();
    while versions.len() > keep {
        let candidate = versions.remove(0);
        if Some(&candidate) == active.as_ref() {
            // Never delete the version Chrome is currently launching.
            versions.push(candidate);
            if versions.len() <= keep {
                break;
            }
            continue;
        }
        let path = version_dir(root, &candidate)?;
        if fs::remove_dir_all(&path).is_ok() {
            removed.push(candidate);
        } else {
            break;
        }
    }
    Ok(removed)
}

/// Reads the version a candidate binary reports. Kept separate from the
/// verification path so a probe failure can never be mistaken for a good build.
pub fn probe(binary: &Path) -> Result<String> {
    let mut command = std::process::Command::new(binary);
    command.arg("--version");
    // 被探测的是控制台程序：不隐藏的话，在 GUI 父进程（Tauri 桌面）下会闪一个黑窗口。
    crate::childproc::hide(&mut command);
    let output = command
        .output()
        .context("staged binary could not be started")?;
    if !output.status.success() {
        bail!("staged binary failed its health check");
    }
    let reported = String::from_utf8_lossy(&output.stdout).trim().to_string();
    if !valid_version(&reported) {
        bail!("staged binary reported an unusable version");
    }
    Ok(reported)
}

/// Health check a staged build must pass before it is allowed to replace the
/// running one: it must start, answer, and speak the same local protocol.
pub fn probe_component(binary: &Path) -> Result<(String, u32, u32)> {
    let mut command = std::process::Command::new(binary);
    command.arg("--component");
    crate::childproc::hide(&mut command);
    let output = command
        .output()
        .context("staged binary could not be started")?;
    if !output.status.success() {
        bail!("staged binary failed its component check");
    }
    parse_component(&output.stdout)
}

/// Kept pure so the health-check contract is covered without a build artifact.
pub fn parse_component(bytes: &[u8]) -> Result<(String, u32, u32)> {
    let value: serde_json::Value = serde_json::from_slice(bytes)?;
    let version = value["version"].as_str().unwrap_or_default().to_string();
    let ipc = value["ipc_version"].as_u64().unwrap_or_default() as u32;
    let journal = value["journal_version"].as_u64().unwrap_or_default() as u32;
    if !valid_version(&version) || ipc != IPC_VERSION || journal != JOURNAL_VERSION {
        bail!("staged binary reports an incompatible component protocol");
    }
    Ok((version, ipc, journal))
}

#[cfg(test)]
mod tests {
    use super::*;
    use p256::ecdsa::{signature::Signer, SigningKey};

    #[test]
    fn the_source_policy_only_widens_the_scheme_and_nothing_else() {
        // 正式通道：明文一律不认。
        assert!(!SourcePolicy::STRICT.valid_source("http://10.9.8.162:6111/channel"));
        assert!(SourcePolicy::STRICT.valid_source("https://updates.example.com/channel"));
        // 开发通道：额外认 http，其余规则一条都不放松。
        assert!(SourcePolicy::DEV.valid_source("http://10.9.8.162:6111/channel"));
        assert!(SourcePolicy::DEV.valid_source("https://updates.example.com/channel"));
        for bad in [
            "http://10.9.8.162:6111/channel/",     // 尾斜杠 = 不是规范 origin
            "http://user@10.9.8.162:6111/channel", // 带用户信息
            "http://10.9.8.162:6111/channel#frag", // 带 fragment
            "ftp://10.9.8.162:6111/channel",       // 别的 scheme
            "10.9.8.162:6111/channel",             // 没有 scheme
        ] {
            assert!(!SourcePolicy::DEV.valid_source(bad), "{bad}");
        }
    }

    #[test]
    fn the_source_policy_never_allows_a_scheme_downgrade() {
        let source = "https://updates.example.com/channel";
        assert!(SourcePolicy::DEV.same_origin(source, "https://updates.example.com/channel/x.zip"));
        // 源是 https 时，包不能走明文 —— 放行明文是指「源本来就在明文通道上」，
        // 不是「允许把 https 通道里的下载降级」。
        assert!(!SourcePolicy::DEV.same_origin(source, "http://updates.example.com/channel/x.zip"));
        // 反过来（源是 http）也不行：混用说明清单被改过。
        let insecure = "http://10.9.8.162:6111/channel";
        assert!(SourcePolicy::DEV.same_origin(insecure, "http://10.9.8.162:6111/channel/x.zip"));
        assert!(!SourcePolicy::DEV.same_origin(insecure, "https://10.9.8.162:6111/channel/x.zip"));
        // 换主机永远不行。
        assert!(!SourcePolicy::DEV.same_origin(insecure, "http://evil.example.com/x.zip"));
    }

    fn key() -> (SigningKey, String) {
        let key = SigningKey::from_slice(&[0x11; 32]).unwrap();
        let point = key.verifying_key().to_encoded_point(false);
        (key, B64.encode(point.as_bytes()))
    }

    fn release(version: &str) -> Release {
        Release {
            version: version.into(),
            platform: platform_id(std::env::consts::OS, std::env::consts::ARCH),
            notes: "notes".into(),
            expires_at: crate::now() + 3600,
            ipc_version: IPC_VERSION,
            journal_version: JOURNAL_VERSION,
            artifacts: vec![
                Artifact {
                    kind: "native-host".into(),
                    url: "https://updates.example.com/cloudfile-native-host".into(),
                    sha256: "a".repeat(64),
                },
                Artifact {
                    kind: "desktop".into(),
                    url: "https://updates.example.com/cloudfile-desktop".into(),
                    sha256: "b".repeat(64),
                },
            ],
            signature: String::new(),
        }
    }

    fn sign(key: &SigningKey, payload: &[u8]) -> String {
        let signature: Signature = key.sign(payload);
        B64.encode(signature.to_bytes())
    }

    fn signed(key: &SigningKey, mut release: Release) -> Release {
        release.signature = sign(key, &signing_payload(&release));
        release
    }

    fn current_platform() -> String {
        platform_id(std::env::consts::OS, std::env::consts::ARCH)
    }

    #[test]
    fn a_fully_valid_release_is_accepted() {
        let (key, pinned) = key();
        let release = signed(&key, release("2.2.1"));
        verify(
            SourcePolicy::STRICT,
            &release,
            "https://updates.example.com",
            "2.2.0",
            crate::now(),
            &current_platform(),
            &pinned,
        )
        .unwrap();
    }

    #[test]
    fn every_tampered_field_is_rejected() {
        let (key, pinned) = key();
        let base = signed(&key, release("2.2.1"));
        let check = |candidate: Release| {
            verify(
                SourcePolicy::STRICT,
                &candidate,
                "https://updates.example.com",
                "2.2.0",
                crate::now(),
                &current_platform(),
                &pinned,
            )
        };
        // Expired.
        let mut expired = base.clone();
        expired.expires_at = 1;
        assert!(check(expired).is_err());
        // Wrong platform.
        let mut platform = base.clone();
        platform.platform = "plan9-sparc".into();
        assert!(check(platform).is_err());
        // Incompatible protocol.
        let mut protocol = base.clone();
        protocol.journal_version = 2;
        assert!(check(protocol).is_err());
        // Not newer.
        let mut older = base.clone();
        older.version = "2.1.9".into();
        assert!(check(older).is_err());
        // Foreign download host.
        let mut foreign = base.clone();
        foreign.artifacts[0].url = "https://evil.example.com/native-host".into();
        assert!(check(foreign).is_err());
        // Missing artifact.
        let mut missing = base.clone();
        missing.artifacts.pop();
        assert!(check(missing).is_err());
        // Malformed digest.
        let mut digest = base.clone();
        digest.artifacts[1].sha256 = "not-a-digest".into();
        assert!(check(digest).is_err());
        // Any content change invalidates the signature.
        let mut tampered = base.clone();
        tampered.notes = "different".into();
        assert!(check(tampered).is_err());
        // A different pinned key is refused.
        let other = SigningKey::from_slice(&[0x22; 32]).unwrap();
        let signature = sign(&other, &signing_payload(&base));
        let mut swapped = base.clone();
        swapped.signature = signature;
        assert!(check(swapped).is_err());
        // Unsigned.
        assert!(check(release("2.2.1")).is_err());
        // The untouched release still passes, so the checks are not blanket-failing.
        assert!(check(base).is_ok());
    }

    #[test]
    fn release_manifest_rejects_unknown_fields_and_oversized_input() {
        assert!(parse_release(br#"{"version":"2.2.1","extra":true}"#).is_err());
        assert!(parse_release(&vec![b' '; MAX_RELEASE_BYTES + 1]).is_err());
    }

    #[test]
    fn versions_order_numerically_and_prereleases_sort_first() {
        assert_eq!(compare("2.10.0", "2.9.9"), Ordering::Greater);
        assert_eq!(compare("2.2.0", "2.2.0-rc.1"), Ordering::Greater);
        assert_eq!(compare("2.2.0-rc.1", "2.2.0"), Ordering::Less);
        assert_eq!(compare("2.2.0", "2.2.0"), Ordering::Equal);
        assert!(valid_version("2.2.0"));
        assert!(valid_version("2.2.0-alpha.2"));
        for bad in ["", "2.2", "2.2.0.1", "v2.2.0", "../evil", "2.2.0-"] {
            assert!(!valid_version(bad), "{bad} must be rejected");
        }
    }

    #[test]
    fn staging_activation_and_prune_keep_a_revertible_history() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.join("install");
        let manifests = base.join("hosts");
        let os = std::env::consts::OS;
        let id = "abcdefghijklmnopabcdefghijklmnop";

        // Register the currently running build first: takeover needs an owner.
        let live = base.join(Binary::NativeHost.file_name(os, "2.2.0"));
        let desktop = base.join(Binary::Desktop.file_name(os, "2.2.0"));
        fs::write(&live, b"old").unwrap();
        fs::write(&desktop, b"old").unwrap();
        hostreg::install(
            &manifests,
            hostreg::DEV_HOST,
            id,
            &live.canonicalize().unwrap(),
            true,
        )
        .unwrap();
        assert_eq!(
            active_version(&manifests, hostreg::DEV_HOST).unwrap(),
            None,
            "a flat install has no version directory"
        );

        stage(
            &root,
            "2.2.1",
            os,
            &[
                (Binary::NativeHost, b"new-host".to_vec()),
                (Binary::Desktop, b"new-desktop".to_vec()),
            ],
        )
        .unwrap();
        assert_eq!(
            installed_versions(&root).unwrap(),
            vec!["2.2.1".to_string()]
        );
        assert!(stage(&root, "2.2.1", os, &[]).is_err());

        activate(&root, &manifests, hostreg::DEV_HOST, "2.2.1").unwrap();
        assert_eq!(
            active_version(&manifests, hostreg::DEV_HOST)
                .unwrap()
                .as_deref(),
            Some("2.2.1")
        );
        let active: serde_json::Value =
            storage::read(&hostreg::manifest_path(&manifests, hostreg::DEV_HOST)).unwrap();
        assert!(active["path"]
            .as_str()
            .unwrap()
            .ends_with(&Binary::NativeHost.file_name(os, "2.2.1")));
        assert_eq!(
            active["allowed_origins"][0],
            format!("chrome-extension://{id}/")
        );

        // A second version, then a revert to the preserved manifest.
        stage(
            &root,
            "2.2.2",
            os,
            &[
                (Binary::NativeHost, b"newer-host".to_vec()),
                (Binary::Desktop, b"newer-desktop".to_vec()),
            ],
        )
        .unwrap();
        activate(&root, &manifests, hostreg::DEV_HOST, "2.2.2").unwrap();
        assert_eq!(
            active_version(&manifests, hostreg::DEV_HOST)
                .unwrap()
                .as_deref(),
            Some("2.2.2")
        );
        rollback(&manifests, hostreg::DEV_HOST).unwrap();
        assert_eq!(
            active_version(&manifests, hostreg::DEV_HOST)
                .unwrap()
                .as_deref(),
            Some("2.2.1")
        );

        // Pruning never removes the active version.
        let removed = prune(&root, hostreg::DEV_HOST, &manifests, 1).unwrap();
        assert_eq!(removed, vec!["2.2.2".to_string()]);
        assert_eq!(
            installed_versions(&root).unwrap(),
            vec!["2.2.1".to_string()]
        );
    }

    #[test]
    fn activation_refuses_an_unregistered_or_incomplete_target() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.join("install");
        let manifests = base.join("hosts");
        let os = std::env::consts::OS;
        stage(
            &root,
            "2.2.1",
            os,
            &[(Binary::NativeHost, b"host".to_vec())],
        )
        .unwrap();
        assert!(activate(&root, &manifests, hostreg::DEV_HOST, "2.2.1").is_err());
        assert!(activate(&root, &manifests, hostreg::DEV_HOST, "2.2.9").is_err());
        // The failed attempt must not have produced a manifest for Chrome.
        assert!(!hostreg::manifest_path(&manifests, hostreg::DEV_HOST).exists());
        assert!(prune(&root, hostreg::DEV_HOST, &manifests, 1)
            .unwrap()
            .is_empty());
    }

    #[test]
    fn component_probe_rejects_an_incompatible_build() {
        let good = br#"{"version":"2.2.1","ipc_version":1,"journal_version":1}"#;
        assert_eq!(
            parse_component(good).unwrap(),
            ("2.2.1".to_string(), IPC_VERSION, JOURNAL_VERSION)
        );
        for bad in [
            &br#"{"version":"2.2.1","ipc_version":2,"journal_version":1}"#[..],
            &br#"{"version":"2.2.1","journal_version":2,"ipc_version":1}"#[..],
            &br#"{"version":"2.2.1"}"#[..],
            &br#"{"version":"broken","ipc_version":1,"journal_version":1}"#[..],
            &b"not json"[..],
        ] {
            assert!(parse_component(bad).is_err());
        }
    }

    #[test]
    fn install_root_is_found_from_a_versioned_binary() {
        let root = Path::new("/opt/cloudfile/agent22");
        let exe = root.join("bin/2.2.1/cloudfile-native-host");
        assert_eq!(install_root(&exe), root);
        let flat = Path::new("/opt/cloudfile/agent22/bin");
        assert_eq!(
            install_root(&flat.join("cloudfile-native-host")),
            Path::new("/opt/cloudfile/agent22")
        );
    }
}
