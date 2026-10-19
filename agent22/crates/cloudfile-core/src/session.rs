use crate::{storage, wire::Request};
use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
    time::Duration,
};
use url::Url;
#[derive(Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub allowed_origins: Vec<String>,
    #[serde(default)]
    pub service_paths: std::collections::BTreeMap<String, String>,
    /// Signed release origin. Both this and the key must be set before an
    /// update can be offered; one without the other is never enough.
    #[serde(default)]
    pub update_source: Option<String>,
    /// Base64url of the pinned uncompressed P-256 release key.
    #[serde(default)]
    pub update_public_key: Option<String>,
    /// Local unpacked extension copy, used only to report its version.
    #[serde(default)]
    pub extension_dir: Option<String>,
    /// 历史审计位：早期只有显式打开它才允许明文 http 更新源。
    ///
    /// 现在 http 是**默认支持**的（内网门户只有 http，测试与正式通道同域名只差路径），
    /// 这个位只在 config.json 里保留「这台机器当初显式开过」的记录，
    /// 读回来不影响任何判定。手写 true/false 都不会改变源能否被接受。
    #[serde(default)]
    pub allow_insecure_update_source: bool,
}

impl Config {
    /// 这台机器上更新源的信任策略。http 与 https 都接受，其余规则（规范 origin、
    /// 同源、sha256、签名）照旧。
    pub fn update_policy(&self) -> crate::update::SourcePolicy {
        crate::update::SourcePolicy {
            allow_insecure: self.allow_insecure_update_source,
        }
    }
}
#[derive(Deserialize)]
pub struct Claimed {
    pub session_id: String,
    pub mode: String,
    pub expires_at: u64,
    pub repo_id: String,
    pub path: String,
    pub file: RemoteFile,
    pub file_id: String,
    pub size: u64,
    pub writeback: Option<serde_json::Value>,
}
#[derive(Deserialize)]
pub struct RemoteFile {
    pub name: String,
    pub content_url: String,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Task {
    #[serde(default)]
    pub device_id: Option<String>,
    pub task_id: String,
    pub file_name: String,
    pub mode: String,
    pub server: String,
    pub repo_id: String,
    pub remote_path: String,
    pub session_id: String,
    pub baseline_file_id: String,
    pub baseline_digest: String,
    pub size: u64,
}
pub fn origin(raw: &str) -> Result<Url> {
    let u = Url::parse(raw)?;
    if !matches!(u.scheme(), "https" | "http")
        || u.host_str().is_none()
        || u.origin().ascii_serialization() != raw
        || raw.len() > 255
    {
        bail!("canonical HTTP(S) origin required");
    }
    Ok(u)
}
pub fn valid_service_path(prefix: &str) -> bool {
    prefix.is_empty()
        || (prefix.starts_with('/')
            && prefix.len() <= 100
            && prefix.split('/').skip(1).all(|part| {
                !part.is_empty()
                    && part
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
            }))
}
pub fn service_path(root: &Path, server: &str) -> Result<String> {
    origin(server)?;
    let prefix = config(root)?
        .service_paths
        .get(server)
        .cloned()
        .unwrap_or_default();
    if !valid_service_path(&prefix) {
        bail!("invalid service path");
    }
    Ok(prefix)
}
/// 1.0 安装器写进 `config.json` 的受信任服务：内网门户 + 直连静态服务。
///
/// 这些是**出厂默认项**，不是用户的选择：升级上来的机器本来就信任它们，
/// 换一套客户端不该让用户再去点一遍。所以它们随首次启动落进配置（[`ensure_defaults`]），
/// 并且**不允许在界面上删除**（[`untrust`] 会拒绝）——「默认」如果可以被删掉，
/// 那它只是一个预填值，而这里要的是「这台机器出厂就信任哪些服务」的稳定事实。
pub const DEFAULT_ORIGINS: &[&str] = &[
    "http://10.9.8.162:6111",
    "http://10.12.1.157:8092",
    "http://etech.stcetech.ad01.sec.com",
    "https://etech.stcetech.ad01.sec.com",
];

/// 这个地址是不是出厂默认项（按规范 origin 比较，带不带尾斜杠、带不带路径都一样）。
pub fn is_default_origin(raw: &str) -> bool {
    let Ok(parsed) = Url::parse(raw) else {
        return false;
    };
    let server = parsed.origin().ascii_serialization();
    DEFAULT_ORIGINS.iter().any(|item| *item == server)
}

/// 把缺的默认服务补进 `config.json`，返回补了几条。
///
/// 幂等：已经是默认项的不会再写一次，所以每次启动调用它都不会改动文件。
/// 只补不删 —— 用户自己加的服务原样保留，顺序也不动（默认项排在最前面，
/// 与 1.0 写出来的形状一致）。
pub fn ensure_defaults(root: &Path) -> Result<usize> {
    let _lock = storage::lock(&root.join("config.lock"))?;
    let mut cfg = config(root)?;
    let mut added = Vec::new();
    for item in DEFAULT_ORIGINS {
        if !cfg.allowed_origins.iter().any(|origin| origin == item) {
            added.push((*item).to_string());
        }
    }
    if added.is_empty() {
        return Ok(0);
    }
    let count = added.len();
    added.extend(cfg.allowed_origins);
    cfg.allowed_origins = added;
    storage::write(&root.join("config.json"), &cfg)?;
    Ok(count)
}

/// 移除一条受信任服务。默认项拒绝移除 —— 与 [`DEFAULT_ORIGINS`] 的说明一致。
pub fn untrust(root: &Path, raw: String) -> Result<()> {
    let parsed = Url::parse(&raw)?;
    let server = parsed.origin().ascii_serialization();
    origin(&server)?;
    if DEFAULT_ORIGINS.iter().any(|item| *item == server) {
        bail!("默认服务不能删除");
    }
    let _lock = storage::lock(&root.join("config.lock"))?;
    let mut cfg = config(root)?;
    cfg.allowed_origins.retain(|item| item != &server);
    cfg.service_paths.remove(&server);
    storage::write(&root.join("config.json"), &cfg)
}

pub fn trust(root: &Path, raw: String) -> Result<()> {
    let parsed = Url::parse(&raw)?;
    let server = parsed.origin().ascii_serialization();
    origin(&server)?;
    let prefix = parsed.path().trim_end_matches('/');
    if parsed.query().is_some()
        || parsed.fragment().is_some()
        || !valid_service_path(prefix)
        || raw.trim_end_matches('/') != format!("{server}{prefix}")
    {
        bail!("canonical service address required");
    }
    let _lock = storage::lock(&root.join("config.lock"))?;
    let mut cfg = config(root)?;
    if !cfg.allowed_origins.contains(&server) {
        cfg.allowed_origins.push(server.clone());
    }
    // Re-adding the origin must not erase a known deployment path. A paired
    // device is origin-bound; an EAP portal root is not the CloudFile API root.
    if !prefix.is_empty() || !cfg.service_paths.contains_key(&server) {
        cfg.service_paths.insert(server, prefix.into());
    }
    storage::write(&root.join("config.json"), &cfg)
}
pub fn config(root: &Path) -> Result<Config> {
    let p = root.join("config.json");
    if !p.exists() {
        return Ok(Config::default());
    }
    storage::read(&p)
}
/// Configures (or clears) the signed update source. Setting a source without a
/// pinned key would make verification impossible, so both move together.
pub fn set_update(
    root: &Path,
    source: Option<String>,
    public_key: Option<String>,
    extension_dir: Option<String>,
) -> Result<()> {
    set_update_with_policy(
        root,
        source,
        public_key,
        extension_dir,
        crate::update::SourcePolicy::STRICT,
    )
}

/// 同上，但按给定策略校验源。开发通道（明文 http）由调用方显式传入
/// [`crate::update::SourcePolicy::DEV`]；策略同时被**记进配置**，
/// 这样以后每次升级用的都是同一套规则，而不是「设置时宽松、升级时严格」。
pub fn set_update_with_policy(
    root: &Path,
    source: Option<String>,
    public_key: Option<String>,
    extension_dir: Option<String>,
    policy: crate::update::SourcePolicy,
) -> Result<()> {
    for value in [&source, &public_key, &extension_dir].into_iter().flatten() {
        if value.len() > 4096 || value.contains('\0') {
            bail!("invalid update configuration");
        }
    }
    if let Some(value) = &source {
        policy.valid_source(value).then_some(()).ok_or_else(|| {
            anyhow::anyhow!("update source must be a canonical http(s) origin")
        })?;
        match &public_key {
            Some(key) => {
                crate::update::verifying_key(key)?;
            }
            // 只有开发通道允许「有源、没有签名公钥」：那种通道服务的是**扩展更新**
            // （扩展不签名，信任锚是同源 + sha256），Agent 自身则明确报「不可升级」。
            // 正式通道仍然要求两条都在，免得出现一个看着配好了、其实验不了签的通道。
            None if policy.allow_insecure => {}
            None => bail!("a pinned release key is required"),
        }
    } else if public_key.is_some() {
        // A key that can never be used would only hide a caller mistake: say so
        // instead of storing a half-configured update channel.
        bail!("an update key requires an update source");
    }
    let _lock = storage::lock(&root.join("config.lock"))?;
    let mut cfg = config(root)?;
    cfg.update_source = source;
    cfg.update_public_key = public_key;
    cfg.extension_dir = extension_dir;
    // 策略跟着源一起存：升级时读回来的是「当初同意的那套规则」。
    cfg.allow_insecure_update_source = policy.allow_insecure && cfg.update_source.is_some();
    // Reaching here the pair is either both set or both clear, so a bare write
    // is enough — there is no half-configured state to normalise away.
    storage::write(&root.join("config.json"), &cfg)
}
pub fn task_id(raw: &str) -> bool {
    raw.len() == 64
        && raw
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
pub fn task_dir(root: &Path, id: &str) -> Result<PathBuf> {
    if !task_id(id) {
        bail!("invalid task ID");
    }
    Ok(crate::workspace::tasks_root(root).join(id))
}
pub fn load(root: &Path, id: &str) -> Result<Task> {
    // 读前加固：任务记录必须是**私有的普通文件**。与 Go 的 store 同一处判断
    // （`internal/taskcenter/store.go`: 非私有 = ErrCorrupt），语义是 **fail closed** ——
    // 一份权限松掉的状态文件不能被当成可信输入去授权退出、上传或续租。
    // Windows 上这一条无从判断（见 fsaccess::private），其余加固照旧。
    let path = task_dir(root, id)?.join("task.json");
    crate::fsaccess::private_file(&path)?;
    let task: Task = storage::read(&path)?;
    if task.task_id != id {
        bail!("task identity mismatch");
    }
    filename(&task.file_name)?;
    Ok(task)
}
pub fn local_path(root: &Path, task: &Task) -> Result<PathBuf> {
    filename(&task.file_name)?;
    let p = task_dir(root, &task.task_id)?.join(&task.file_name);
    if !fs::symlink_metadata(&p)?.is_file() {
        bail!("working copy is not a regular file");
    }
    Ok(p)
}
pub fn tasks(root: &Path) -> Result<Vec<Task>> {
    let dir = crate::workspace::tasks_root(root);
    if !dir.exists() {
        return Ok(vec![]);
    }
    // 任务根必须私有：对应 Go 的 `store.go`「非私有任务目录 = 硬错」。
    // 这里**不是**跳过，而是报错 —— 悄悄少列一个任务会让用户以为工作副本丢了，
    // 而真实原因是权限被人改松了，那正是需要说出来的事。
    crate::fsaccess::private_directory(&dir)?;
    let mut result = Vec::new();
    for entry in fs::read_dir(&dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }
        let id = entry.file_name().to_string_lossy().into_owned();
        if task_id(&id) && entry.path().join("task.json").exists() {
            // 单个任务目录也各自要求私有：任务根是 0700 不代表里面的目录没被改过。
            crate::fsaccess::private_directory(&entry.path())?;
            result.push(load(root, &id)?);
        }
    }
    result.sort_by(|a, b| a.file_name.cmp(&b.file_name));
    Ok(result)
}
fn filename(name: &str) -> Result<()> {
    let stem = name.split('.').next().unwrap_or("").to_ascii_uppercase();
    if name.is_empty()
        || name.len() > 180
        || name.ends_with([' ', '.'])
        || name
            .chars()
            .any(|c| c.is_control() || "<>:\"/\\|?*".contains(c))
        || name == "."
        || name == ".."
        || ["CON", "PRN", "AUX", "NUL"].contains(&stem.as_str())
        || ((stem.starts_with("COM") || stem.starts_with("LPT"))
            && stem.len() == 4
            && stem.as_bytes()[3].is_ascii_digit())
    {
        bail!("unsafe file name");
    }
    Ok(())
}
pub fn digest(path: &Path) -> Result<String> {
    let mut file = fs::File::open(path)?;
    let mut hash = Sha256::new();
    let mut buffer = [0; 65536];
    loop {
        let n = file.read(&mut buffer)?;
        if n == 0 {
            break;
        }
        hash.update(&buffer[..n]);
    }
    Ok(format!("{:x}", hash.finalize()))
}
pub fn open_session(root: &Path, request: Request) -> Result<Task> {
    let Request::Open {
        protocol,
        server,
        ticket,
        expires_at,
        local_action,
    } = request
    else {
        bail!("session required")
    };
    if protocol != "cloudfile-local/v2"
        || expires_at <= crate::now()
        || ticket.is_empty()
        || ticket.len() > 16384
        || !matches!(local_action.as_str(), "" | "keep" | "open" | "overwrite")
    {
        bail!("invalid or expired session");
    }
    let base = origin(&server)?;
    if !config(root)?.allowed_origins.contains(&server) {
        bail!("origin is not trusted; add it in the desktop app");
    }
    let client = reqwest::blocking::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(Duration::from_secs(300))
        .build()?;
    let response = client
        .post(format!(
            "{server}{}/api/v2.1/cloudfile/agent-sessions/claim/",
            service_path(root, &server)?
        ))
        .json(&serde_json::json!({"ticket":ticket}))
        .send()
        .context("session claim failed")?;
    if response.status() != 200 {
        bail!("session claim rejected");
    }
    let mut data = Vec::new();
    response.take(1024 * 1024 + 1).read_to_end(&mut data)?;
    if data.len() > 1024 * 1024 {
        bail!("claim response too large");
    }
    let c: Claimed = serde_json::from_slice(&data)?;
    filename(&c.file.name)?;
    if c.expires_at <= crate::now()
        || !matches!(c.mode.as_str(), "local-view" | "local-edit")
        || c.writeback.is_some()
        || c.size > 4 * 1024 * 1024 * 1024
        || uuid::Uuid::parse_str(&c.repo_id).is_err()
        || uuid::Uuid::parse_str(&c.session_id).is_err()
        || !c.path.starts_with('/')
        || c.path
            .split('/')
            .skip(1)
            .any(|x| x.is_empty() || x == "." || x == "..")
        || c.path.contains(['\\', '\0'])
        || c.file_id.len() != 40
        || !c.file_id.bytes().all(|b| b.is_ascii_hexdigit())
    {
        bail!("invalid session response");
    }
    let content = Url::parse(&c.file.content_url)?;
    if content.origin() != base.origin()
        || !content.username().is_empty()
        || content.password().is_some()
        || content.fragment().is_some()
    {
        bail!("untrusted content URL");
    }
    let id = format!(
        "{:x}",
        Sha256::digest(format!("{server}\0{}\0{}\0{}", c.repo_id, c.mode, c.path))
    );
    let dir = task_dir(root, &id)?;
    storage::private_dir(&dir)?;
    let _lock = storage::lock(&dir.join("operation.lock"))?;
    if dir.join("task.json").exists() {
        // Reopening never replaces an existing working copy, including overwrite requests.
        let task = load(root, &id)?;
        local_path(root, &task)?;
        return Ok(task);
    }
    let target = dir.join(&c.file.name);
    if target.exists() {
        bail!("untracked working copy retained; review the task directory");
    }
    let mut response = client.get(content).send().context("download failed")?;
    if response.status() != 200 {
        bail!("download rejected");
    }
    let mut tmp = tempfile::NamedTempFile::new_in(&dir)?;
    let written = std::io::copy(&mut (&mut response).take(c.size + 1), &mut tmp)?;
    if written != c.size {
        bail!("download size mismatch");
    }
    tmp.flush()?;
    tmp.as_file().sync_all()?;
    let hash = digest(tmp.path())?;
    if c.mode == "local-view" {
        let mut permissions = tmp.as_file().metadata()?.permissions();
        permissions.set_readonly(true);
        tmp.as_file().set_permissions(permissions)?;
    }
    tmp.persist_noclobber(&target)?;
    let task = Task {
        device_id: None,
        task_id: id,
        file_name: c.file.name,
        mode: c.mode,
        server,
        repo_id: c.repo_id,
        remote_path: c.path,
        session_id: c.session_id,
        baseline_file_id: c.file_id,
        baseline_digest: hash,
        size: written,
    };
    storage::write(&dir.join("task.json"), &task)?;
    Ok(task)
}
/// Hands the working copy to a local application. Precedence is an explicit
/// user rule, then a detected application, then the OS default for known
/// document types; an unknown type is never launched implicitly.
pub fn launch(root: &Path, id: &str) -> Result<()> {
    let task = load(root, id)?;
    let path = local_path(root, &task)?;
    crate::apps::open_path(root, &task.mode, &task.file_name, &path)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reject_paths_and_origins() {
        for x in ["../a", "CON.docx", "a:b", "a\\b", ".", "x."] {
            assert!(filename(x).is_err());
        }
        for x in [
            "https://x/",
            "https://x@evil",
            "https://x/path",
            "file:///tmp/a",
        ] {
            assert!(origin(x).is_err());
        }
        assert!(origin("https://example.com").is_ok());
    }

    const DEV_SOURCE: &str = "http://10.9.8.162:6111/cloudfile-updates-dev";

    #[test]
    fn the_default_services_are_seeded_once_and_cannot_be_removed() {
        let (_dir, root) = storage::test_root();
        // 全新机器：先是空的，补一次之后正好是出厂那几条，且默认项排在最前面。
        assert!(config(&root).unwrap().allowed_origins.is_empty());
        assert_eq!(ensure_defaults(&root).unwrap(), DEFAULT_ORIGINS.len());
        let seeded: Vec<String> = DEFAULT_ORIGINS.iter().map(|item| item.to_string()).collect();
        assert_eq!(config(&root).unwrap().allowed_origins, seeded);

        // 幂等：再跑一次不写、不加（每次启动都会调它）。
        assert_eq!(ensure_defaults(&root).unwrap(), 0);

        // 用户自己加的服务保留，默认项仍在最前 —— 界面上「默认」标记依赖这个顺序。
        trust(&root, "https://files.example.com/service".into()).unwrap();
        let saved = config(&root).unwrap();
        assert_eq!(&saved.allowed_origins[..DEFAULT_ORIGINS.len()], &seeded[..]);

        // 默认项不能删（带不带尾斜杠都是同一项）；自己加的可以。
        assert!(untrust(&root, format!("{}/", DEFAULT_ORIGINS[1])).is_err());
        assert!(is_default_origin(&format!("{}/", DEFAULT_ORIGINS[0])));
        assert!(!is_default_origin("https://files.example.com"));
        untrust(&root, "https://files.example.com".into()).unwrap();
        let after = config(&root).unwrap();
        assert!(!after
            .allowed_origins
            .iter()
            .any(|item| item == "https://files.example.com"));
        assert_eq!(after.allowed_origins.len(), DEFAULT_ORIGINS.len());
    }

    #[test]
    fn a_plain_http_source_is_accepted_without_any_extra_switch() {
        let (_dir, root) = storage::test_root();
        // 明文源本身已经不构成拒绝理由（内网通道全是 http，见 update::SourcePolicy）。
        // 这里失败的真正原因是「有源、没公钥」那条规则，而且**什么都没写进去**
        // （不留半配置状态）。
        assert!(set_update(&root, Some(DEV_SOURCE.into()), None, None).is_err());
        assert!(config(&root).unwrap().update_source.is_none());

        // 扩展更新通道（扩展不签名）走显式策略：允许有源无公钥，并把这条决定记进配置。
        set_update_with_policy(
            &root,
            Some(DEV_SOURCE.into()),
            None,
            None,
            crate::update::SourcePolicy::DEV,
        )
        .unwrap();
        let saved = config(&root).unwrap();
        assert_eq!(saved.update_source.as_deref(), Some(DEV_SOURCE));
        assert!(saved.allow_insecure_update_source);
        assert_eq!(saved.update_policy(), crate::update::SourcePolicy::DEV);

        // 清空时开关一并归零：不留一个「开着口子却没有源」的状态。
        set_update(&root, None, None, None).unwrap();
        let cleared = config(&root).unwrap();
        assert!(!cleared.allow_insecure_update_source);
        assert_eq!(cleared.update_policy(), crate::update::SourcePolicy::STRICT);
    }

    #[test]
    fn the_strict_policy_still_requires_a_pinned_key() {
        let (_dir, root) = storage::test_root();
        // 正式通道：只给源不给公钥 = 一个验不了签的通道，拒绝。
        assert!(set_update(
            &root,
            Some("https://updates.example.com".into()),
            None,
            None
        )
        .is_err());
        // 开发通道允许：那种通道只服务扩展更新（扩展不签名），Agent 自身会明确报「不可升级」。
        set_update_with_policy(
            &root,
            Some("https://updates.example.com".into()),
            None,
            None,
            crate::update::SourcePolicy::DEV,
        )
        .unwrap();
        let saved = config(&root).unwrap();
        assert!(saved.update_public_key.is_none());
        assert_eq!(
            saved.update_source.as_deref(),
            Some("https://updates.example.com")
        );
        // 「有公钥没源」永远拒绝（保持原契约）。
        assert!(set_update(&root, None, Some("x".into()), None).is_err());
    }
}
