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
    for value in [&source, &public_key, &extension_dir].into_iter().flatten() {
        if value.len() > 4096 || value.contains('\0') {
            bail!("invalid update configuration");
        }
    }
    if let Some(value) = &source {
        crate::update::valid_source(value)
            .then_some(())
            .ok_or_else(|| anyhow::anyhow!("update source must be a canonical https origin"))?;
        crate::update::verifying_key(
            public_key
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("a pinned release key is required"))?,
        )?;
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
    let task: Task = storage::read(&task_dir(root, id)?.join("task.json"))?;
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
    let mut result = Vec::new();
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }
        let id = entry.file_name().to_string_lossy().into_owned();
        if task_id(&id) && entry.path().join("task.json").exists() {
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
}
