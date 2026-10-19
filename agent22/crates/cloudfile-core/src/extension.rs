//! 浏览器扩展的自更新（对齐 Go 侧 `internal/update/extension.go`）。
//!
//! Chrome 不能热替换已加载的扩展，所以这里只做**把新版本落到磁盘上**这一件事：
//! 扩展自己在 `--status` 里看到 `extension_latest_version == extension_installed_version`
//! 之后调用 `chrome.runtime.reload()` 生效。因此本模块没有「完成通知」，只有
//! 「文件已就位」。
//!
//! 与 Agent 自身的更新（[`crate::update`]）有两处刻意的不同：
//!
//! 1. **同一份更新源**，但清单是同级的 `extension-update.json`，且**不需要签名**——
//!    与 Go 侧一致：信任锚是 https 同源 + sha256，扩展包不是可执行文件，装错了也
//!    不会在用户机器上执行任意代码（Chrome 的权限模型才是最后一道关）。
//! 2. **不做进程内替换**，而是解到临时目录、校验版本、再整体改名换过去。目录布局由
//!    Chrome 加载（`--load-extension`）持有，所以「要么旧的、要么新的」必须原子，
//!    半截目录会让扩展加载失败。

use crate::update;
use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::path::{Component, Path, PathBuf};

/// 解压后放在 agent 根目录下的子目录名（`<agent 根>\extension`）。
///
/// 引导页的「安装扩展」那一步要指给 Chrome 的就是这个目录，所以它必须是一个
/// **可推导**的默认值，而不是只能由 `config.json` 的 `extension_dir` 决定。
pub const DIR_NAME: &str = "extension";

/// 与 Agent 清单同级、固定名字。
///
/// 与 Agent 清单不同，扩展清单**只有一份**：`zip_url` 是相对路径（相对清单地址解析），
/// 所以域名源与 IP 源可以共用同一个文件 —— 不用像 Agent 那样按 host 分两份。
pub const MANIFEST_FILE: &str = "extension-update.json";
/// 清单本身很小：1 MiB 足够，且能挡住往内存里灌垃圾。
const MAX_MANIFEST_BYTES: usize = 1 << 20;
/// 解包后的总体积上限（对齐 Go 的 256 MiB）。
pub const MAX_UNPACKED_BYTES: u64 = 256 << 20;
/// 备份目录后缀（对齐 Go 的 `dir + ".prior"`）。
const BACKUP_SUFFIX: &str = ".prior";

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Release {
    pub version: String,
    pub zip_url: String,
    pub sha256: String,
    #[serde(default)]
    pub notes: String,
}

/// 扩展清单的地址：更新源同级、固定文件名。`policy` 与 Agent 自身升级共用同一套
/// 判定（http 与 https 都接受，规范 origin + 同源一条不放松）。
pub fn manifest_url(policy: update::SourcePolicy, agent_source: &str) -> Result<String> {
    if !policy.valid_source(agent_source) {
        bail!("update source must be a canonical http(s) origin");
    }
    Ok(format!("{agent_source}/{MANIFEST_FILE}"))
}

/// 解包归档的地址。`zip_url` 是相对清单解析的（通道里就是裸文件名），
/// 解析结果必须与更新源**同源**，否则一个被改过的清单就能把下载指到别处。
pub fn archive_url(
    policy: update::SourcePolicy,
    agent_source: &str,
    zip_url: &str,
) -> Result<String> {
    let base = url::Url::parse(&manifest_url(policy, agent_source)?)?;
    let target = base
        .join(zip_url)
        .context("extension archive url is not a valid url")?;
    if !policy.same_origin(agent_source, target.as_str()) {
        bail!("extension archive must live on the update source");
    }
    Ok(target.to_string())
}

pub fn parse_release(bytes: &[u8]) -> Result<Release> {
    let release: Release = serde_json::from_slice(bytes)?;
    validate(&release)?;
    Ok(release)
}

/// 清单字段的校验。写盘前与读盘后都走这里，免得「下载时宽松、安装时严格」。
fn validate(release: &Release) -> Result<()> {
    if !update::valid_version(&release.version) {
        bail!("extension manifest version is not a dotted version");
    }
    if !update::valid_sha256(&release.sha256) {
        bail!("extension manifest sha256 must be 64 lowercase hex characters");
    }
    if release.zip_url.trim().is_empty() {
        bail!("extension manifest has no archive url");
    }
    Ok(())
}

/// 取扩展清单。与 Agent 清单一样不跟随重定向：重定向会把信任锚带走。
pub fn fetch_release(policy: update::SourcePolicy, agent_source: &str) -> Result<Release> {
    let url = manifest_url(policy, agent_source)?;
    let client = reqwest::blocking::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(std::time::Duration::from_secs(20))
        .build()?;
    let response = client
        .get(&url)
        .send()
        .context("extension manifest request failed")?;
    if response.status() != 200 {
        bail!("extension manifest rejected");
    }
    let mut bytes = Vec::new();
    use std::io::Read;
    response
        .take(MAX_MANIFEST_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > MAX_MANIFEST_BYTES {
        bail!("extension manifest is too large");
    }
    parse_release(&bytes)
}

/// 磁盘上那份扩展的版本号（`<dir>/manifest.json` 的 `version`）。
/// 读不到、或版本号不是点分版本，一律 `None`：调用方据此认为「这台机器上还没有
/// 可用的扩展副本」，而不是拿一个坏版本去比较。
pub fn installed_version(dir: &Path) -> Option<String> {
    let bytes = std::fs::read(dir.join("manifest.json")).ok()?;
    let value: serde_json::Value = serde_json::from_slice(&bytes).ok()?;
    let version = value["version"].as_str()?;
    update::valid_version(version).then(|| version.to_string())
}

/// 备份目录：上一个版本在替换期间放在这里。
pub fn backup_path(dir: &Path) -> PathBuf {
    PathBuf::from(format!("{}{BACKUP_SUFFIX}", dir.display()))
}

/// 下载、校验、解包并原子替换。返回安装后的目录。
pub fn apply(
    policy: update::SourcePolicy,
    agent_source: &str,
    release: &Release,
    dir: &Path,
) -> Result<PathBuf> {
    // 先把清单与目标检查完，再动网络：坏参数不该换来一次下载。
    validate(release)?;
    if dir.as_os_str().is_empty() || !dir.is_absolute() {
        bail!("extension directory must be an absolute path");
    }
    let url = archive_url(policy, agent_source, &release.zip_url)?;
    let archive = update::download(&update::Artifact {
        kind: "extension".to_string(),
        url,
        sha256: release.sha256.clone(),
    })?;
    install_archive(&archive, release, dir)
}

/// 从已经校验过 sha256 的归档字节安装。拆出来是为了让「解包 + 换目录」这一步
/// 能在测试里不碰网络地跑（Go 侧用 httptest 覆盖，Rust 侧测试不联网）。
pub fn install_archive(archive: &[u8], release: &Release, dir: &Path) -> Result<PathBuf> {
    let parent = dir.parent().context("extension directory has no parent")?;
    std::fs::create_dir_all(parent)
        .with_context(|| format!("cannot create {}", parent.display()))?;
    let backup = backup_path(dir);
    if backup.exists() {
        // 上一次替换没走完：先让人看一眼，别把上一版备份盖掉。
        bail!(
            "previous extension backup is still present: {}",
            backup.display()
        );
    }

    let staging = tempfile::Builder::new()
        .prefix(".cf-extension-new-")
        .tempdir_in(parent)
        .context("cannot create a staging directory")?;
    let unpacked = unpack(archive, staging.path())?;
    // 版本必须与清单一致：清单说 1.0.5、包里是 1.0.4 的话，扩展会陷入
    // 「看到新版本 → 重载 → 还是旧版本」的循环。
    match unpacked.as_deref() {
        Some(version) if version == release.version => {}
        Some(version) => bail!(
            "extension archive reports version {version}, expected {}",
            release.version
        ),
        None => bail!("extension archive has no readable manifest.json"),
    }

    let had_previous = dir.exists();
    if had_previous {
        std::fs::rename(dir, &backup).context("cannot set the previous extension aside")?;
    }
    if let Err(error) = std::fs::rename(staging.path(), dir) {
        // 换不过去就把它放回原位：宁可留着旧版本，也别留下一个空目录。
        if had_previous {
            let _ = std::fs::rename(&backup, dir);
        }
        return Err(error).context("cannot move the new extension into place");
    }
    if had_previous {
        let _ = std::fs::remove_dir_all(&backup);
    }
    Ok(dir.to_path_buf())
}

/// 解包到 `target`，返回归档内 `manifest.json` 声明的版本号。
///
/// 归档来自网络，按**不可信输入**处理：绝对路径、`..`、反斜杠分隔（Windows 上的
/// zip slip）、符号链接、非普通文件、超出体积上限，一律拒绝。解包只写新建文件
/// （`create_new`），所以永远不会覆盖已有内容。
fn unpack(archive: &[u8], target: &Path) -> Result<Option<String>> {
    use std::io::Read;
    let mut zip = zip::ZipArchive::new(std::io::Cursor::new(archive))
        .context("extension archive is not a readable zip")?;
    let mut total: u64 = 0;
    for index in 0..zip.len() {
        let mut entry = zip.by_index(index)?;
        let name = entry
            .name()
            .with_context(|| format!("extension archive member {index} has an unreadable name"))?
            .to_string();
        let relative = relative_member_path(&name)?;
        let mode = entry.unix_mode();
        if let Some(mode) = mode {
            let kind = mode & 0o170000;
            // 0o100000 = 普通文件；0 = 归档里根本没写类型位（Python/Windows 产物
            // 只有权限位），同样按普通文件处理。其余（符号链接 0o120000、FIFO 等）
            // 一律拒绝。
            if kind != 0o100000 && kind != 0 {
                bail!("invalid extension archive member: {name}");
            }
        }
        if entry.is_dir() {
            std::fs::create_dir_all(target.join(&relative))?;
            continue;
        }
        total = total.saturating_add(entry.size());
        if total > MAX_UNPACKED_BYTES {
            bail!("extension archive exceeds the unpacked size limit");
        }
        let path = target.join(&relative);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let mut file = std::fs::File::create_new(&path)
            .with_context(|| format!("cannot write {}", path.display()))?;
        // 声明的长度可能是假的：只读 size+1 字节，读满 size 才算数。
        let declared = entry.size();
        let copied = std::io::copy(&mut (&mut entry).take(declared + 1), &mut file)
            .with_context(|| format!("cannot unpack {}", relative.display()))?;
        drop(file);
        if copied != declared {
            bail!("extension archive member length mismatch: {name}");
        }
    }
    Ok(installed_version(target))
}

/// 归档内成员的相对路径。`Path::components()` 在 Windows 上也认反斜杠，所以
/// `..\evil` 无论在哪台机器上解包都会被拒绝。
fn relative_member_path(name: &str) -> Result<PathBuf> {
    if name.contains('\\') {
        bail!("invalid extension archive path: {name}");
    }
    let mut relative = PathBuf::new();
    for component in Path::new(name).components() {
        match component {
            Component::Normal(part) => relative.push(part),
            // `./x` 与 `x` 是同一个位置，Go 侧 Clean 之后也是这么处理的。
            Component::CurDir => {}
            _ => bail!("invalid extension archive path: {name}"),
        }
    }
    if relative.as_os_str().is_empty() {
        bail!("invalid extension archive path: {name}");
    }
    Ok(relative)
}

#[cfg(test)]
mod tests {
    use super::*;

    const SOURCE: &str = "https://updates.example.com/channel";

    fn zip_of(entries: &[(&str, &[u8])]) -> Vec<u8> {
        let mut buffer = std::io::Cursor::new(Vec::new());
        {
            let mut writer = zip::ZipWriter::new(&mut buffer);
            for (name, body) in entries {
                writer
                    .start_file::<_, ()>(*name, zip::write::SimpleFileOptions::default())
                    .unwrap();
                std::io::Write::write_all(&mut writer, body).unwrap();
            }
            writer.finish().unwrap();
        }
        buffer.into_inner()
    }

    fn manifest(version: &str) -> Vec<u8> {
        format!(r#"{{"manifest_version":3,"name":"CloudFile","version":"{version}"}}"#).into_bytes()
    }

    fn release(version: &str, sha256: &str) -> Release {
        Release {
            version: version.to_string(),
            zip_url: "cloudfile-local-session-receiver.zip".to_string(),
            sha256: sha256.to_string(),
            notes: String::new(),
        }
    }

    #[test]
    fn the_manifest_sits_next_to_the_agent_manifest() {
        assert_eq!(
            manifest_url(update::SourcePolicy::STRICT, SOURCE).unwrap(),
            "https://updates.example.com/channel/extension-update.json"
        );
        // 源必须是规范 origin；scheme 不限（内网通道只有明文 http）。
        assert_eq!(
            manifest_url(
                update::SourcePolicy::STRICT,
                "http://updates.example.com/channel"
            )
            .unwrap(),
            "http://updates.example.com/channel/extension-update.json"
        );
        assert!(manifest_url(
            update::SourcePolicy::STRICT,
            "https://updates.example.com/channel/"
        )
        .is_err());
        assert!(manifest_url(
            update::SourcePolicy::STRICT,
            "https://user@updates.example.com"
        )
        .is_err());
    }

    #[test]
    fn the_plain_http_channel_is_usable_without_widening_anything_else() {
        // 就是测试通道的真实地址（明文 http + 通道目录）。
        const DEV_SOURCE: &str = "http://10.9.8.162:6111/cloudfile-updates-dev";
        // 明文 http 默认就支持：两个策略常量拿到的地址完全一样。
        assert_eq!(
            manifest_url(update::SourcePolicy::STRICT, DEV_SOURCE).unwrap(),
            "http://10.9.8.162:6111/cloudfile-updates-dev/extension-update.json"
        );
        assert_eq!(
            manifest_url(update::SourcePolicy::DEV, DEV_SOURCE).unwrap(),
            "http://10.9.8.162:6111/cloudfile-updates-dev/extension-update.json"
        );
        // 包仍然按同源解析，跨源/跨 scheme 照样拒绝。
        assert_eq!(
            archive_url(update::SourcePolicy::DEV, DEV_SOURCE, "receiver.zip").unwrap(),
            "http://10.9.8.162:6111/cloudfile-updates-dev/receiver.zip"
        );
        assert!(archive_url(
            update::SourcePolicy::DEV,
            DEV_SOURCE,
            "https://updates.example.com/receiver.zip"
        )
        .is_err());
        assert!(archive_url(
            update::SourcePolicy::DEV,
            DEV_SOURCE,
            "http://10.9.8.199:6111/receiver.zip"
        )
        .is_err());
        // 放行 scheme 不等于放行「随便写的源」。
        assert!(manifest_url(update::SourcePolicy::DEV, "http://10.9.8.162:6111/dev/").is_err());
    }

    #[test]
    fn the_archive_is_resolved_relative_to_the_manifest_and_stays_on_source() {
        // 通道里的写法：裸文件名。
        assert_eq!(
            archive_url(
                update::SourcePolicy::STRICT,
                SOURCE,
                "cloudfile-local-session-receiver-1.0.4-dev.zip"
            )
            .unwrap(),
            "https://updates.example.com/channel/cloudfile-local-session-receiver-1.0.4-dev.zip"
        );
        // Go 侧的 ./ 写法。
        assert_eq!(
            archive_url(update::SourcePolicy::STRICT, SOURCE, "./receiver.zip").unwrap(),
            "https://updates.example.com/channel/receiver.zip"
        );
        // 换源 = 拒绝：清单被改过时不能把下载指到别处。
        assert!(archive_url(
            update::SourcePolicy::STRICT,
            SOURCE,
            "https://evil.example.com/receiver.zip"
        )
        .is_err());
        assert!(archive_url(
            update::SourcePolicy::STRICT,
            SOURCE,
            "http://updates.example.com/receiver.zip"
        )
        .is_err());
    }

    #[test]
    fn the_real_channel_manifests_parse() {
        // 逐字取自 cloudfile-updates-dev / cloudfile-updates 两个通道。
        let dev = br#"{"version":"1.0.4","zip_url":"cloudfile-local-session-receiver-1.0.4-dev.zip","sha256":"17c5bd92d6559b077c337606b2d88d3d470dec9e87428940458559788044e74d","notes":"local dev build 2026-10-14 17:18"}"#;
        let release = parse_release(dev).unwrap();
        assert_eq!(release.version, "1.0.4");
        assert_eq!(release.sha256.len(), 64);
        let prod = br#"{"version":"1.0.0","sha256":"60625da0952a8a9c7b3d9b9894acfa738de53a93c8c631128796a18a1a9f15eb","zip_url":"cloudfile-local-session-receiver-1.0.0.zip","notes":"release 1.0.0"}"#;
        assert_eq!(parse_release(prod).unwrap().version, "1.0.0");
        // notes 缺席也接受（字段是可选的），但版本号与摘要不合法就拒绝。
        assert!(parse_release(br#"{"version":"1.0","zip_url":"x.zip","sha256":"60625da0952a8a9c7b3d9b9894acfa738de53a93c8c631128796a18a1a9f15eb"}"#).is_err());
        assert!(
            parse_release(br#"{"version":"1.0.0","zip_url":"x.zip","sha256":"short"}"#).is_err()
        );
        assert!(parse_release(br#"{"version":"1.0.0","sha256":"60625da0952a8a9c7b3d9b9894acfa738de53a93c8c631128796a18a1a9f15eb"}"#).is_err());
        assert!(parse_release(br#"{"version":"1.0.0","zip_url":"x.zip","sha256":"60625da0952a8a9c7b3d9b9894acfa738de53a93c8c631128796a18a1a9f15eb","extra":1}"#).is_err());
    }

    #[test]
    fn the_installed_version_comes_from_the_manifest_on_disk() {
        let directory = tempfile::tempdir().unwrap();
        assert_eq!(installed_version(directory.path()), None);
        std::fs::write(directory.path().join("manifest.json"), manifest("1.0.3")).unwrap();
        assert_eq!(
            installed_version(directory.path()).as_deref(),
            Some("1.0.3")
        );
        // 坏 JSON、缺字段、乱七八糟的版本号一律当作「没有」。
        std::fs::write(directory.path().join("manifest.json"), b"{ not json").unwrap();
        assert_eq!(installed_version(directory.path()), None);
        std::fs::write(directory.path().join("manifest.json"), b"{\"name\":\"x\"}").unwrap();
        assert_eq!(installed_version(directory.path()), None);
        std::fs::write(directory.path().join("manifest.json"), manifest("v1")).unwrap();
        assert_eq!(installed_version(directory.path()), None);
    }

    #[test]
    fn installing_replaces_the_previous_copy_and_removes_the_backup() {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("extension");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("manifest.json"), manifest("1.0.3")).unwrap();
        std::fs::write(dir.join("stale.js"), b"old").unwrap();

        let archive = zip_of(&[
            ("manifest.json", &manifest("1.0.4")),
            ("background.js", b"new"),
        ]);
        let installed =
            install_archive(&archive, &release("1.0.4", &"a".repeat(64)), &dir).unwrap();

        assert_eq!(installed, dir);
        assert_eq!(installed_version(&dir).as_deref(), Some("1.0.4"));
        assert_eq!(std::fs::read(dir.join("background.js")).unwrap(), b"new");
        // 旧内容必须消失，否则会留下上一版的文件。
        assert!(!dir.join("stale.js").exists());
        // 备份是临时的：成功后不留。
        assert!(!backup_path(&dir).exists());
    }

    #[test]
    fn a_first_install_has_no_backup_to_worry_about() {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("extension");
        let archive = zip_of(&[("manifest.json", &manifest("1.0.4"))]);
        install_archive(&archive, &release("1.0.4", &"b".repeat(64)), &dir).unwrap();
        assert!(dir.join("manifest.json").is_file());
        assert!(!backup_path(&dir).exists());
    }

    #[test]
    fn a_mismatched_or_broken_archive_never_touches_the_installed_copy() {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("extension");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("manifest.json"), manifest("1.0.3")).unwrap();

        // 清单说 1.0.4、包里是 1.0.5。
        let wrong = zip_of(&[("manifest.json", &manifest("1.0.5"))]);
        assert!(install_archive(&wrong, &release("1.0.4", &"c".repeat(64)), &dir).is_err());
        // 没有 manifest.json 的包。
        let empty = zip_of(&[("background.js", b"x")]);
        assert!(install_archive(&empty, &release("1.0.4", &"d".repeat(64)), &dir).is_err());
        // 不是 zip。
        assert!(install_archive(b"not a zip", &release("1.0.4", &"e".repeat(64)), &dir).is_err());

        // 三次失败之后，旧的副本原封不动。
        assert_eq!(installed_version(&dir).as_deref(), Some("1.0.3"));
        assert!(!backup_path(&dir).exists());
    }

    #[test]
    fn archive_paths_cannot_escape_the_extension_directory() {
        // zip slip：绝对路径、`..`、Windows 反斜杠。
        for name in [
            "../escape.js",
            "a/../../escape.js",
            "/absolute.js",
            "..\\escape.js",
            "a\\..\\..\\escape.js",
        ] {
            let archive = zip_of(&[(name, b"x")]);
            let base = tempfile::tempdir().unwrap();
            let dir = base.path().join("extension");
            let error = install_archive(&archive, &release("1.0.4", &"f".repeat(64)), &dir)
                .expect_err(name);
            let message = error.to_string();
            assert!(
                message.contains("invalid extension archive path") || message.contains("manifest"),
                "{name}: {message}"
            );
            // 逃逸目标没有被创建。
            assert!(!base.path().join("escape.js").exists());
            assert!(!base.path().parent().unwrap().join("escape.js").exists());
        }
    }

    #[test]
    fn a_leftover_backup_blocks_the_swap_until_a_human_looks() {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("extension");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::create_dir_all(backup_path(&dir)).unwrap();
        let archive = zip_of(&[("manifest.json", &manifest("1.0.4"))]);
        let error =
            install_archive(&archive, &release("1.0.4", &"1".repeat(64)), &dir).unwrap_err();
        assert!(error.to_string().contains("backup"));
        // 旧副本还在原位，没有被覆盖。
        assert!(dir.exists());
    }

    #[test]
    fn applying_a_foreign_archive_url_fails_before_any_download() {
        let base = tempfile::tempdir().unwrap();
        let mut release = release("1.0.4", &"2".repeat(64));
        release.zip_url = "https://evil.example.com/receiver.zip".to_string();
        let error = apply(
            update::SourcePolicy::STRICT,
            SOURCE,
            &release,
            &base.path().join("extension"),
        )
        .unwrap_err();
        assert!(error.to_string().contains("update source"), "{error}");
    }
}
