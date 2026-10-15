use crate::{
    device::{self, Client},
    exclusive,
    session::{self, Task},
    storage,
};
use anyhow::{bail, Result};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    io::{Read, Write},
    path::Path,
};
#[derive(Deserialize)]
struct Transfer {
    path: String,
    method: String,
    authorization: String,
    redirects: bool,
    resume: bool,
}
#[derive(Deserialize)]
struct File {
    extension: String,
    mode: String,
    base_version: String,
}
#[derive(Deserialize)]
struct Grant {
    ticket: String,
    expires_in: u64,
    transfer: Transfer,
    file: File,
}
pub struct Open {
    pub instance: String,
    pub session_id: String,
    pub ticket: String,
}
pub fn parse(uri: &str) -> Result<Open> {
    if uri.len() > 2048 || !uri.starts_with("cloudfile-open://v1/open?instance=") {
        bail!("无效的本地编辑链接");
    }
    let url = url::Url::parse(uri)?;
    let args = url.query_pairs().collect::<Vec<_>>();
    if url.fragment().is_some()
        || args.len() != 3
        || args[0].0 != "instance"
        || args[1].0 != "session"
        || args[2].0 != "ticket"
    {
        bail!("无效的本地编辑链接");
    }
    session::origin(&args[0].1)?;
    if !device::uuid(&args[1].1) || !device::encoded(&args[2].1, 32) {
        bail!("无效的编辑会话");
    }
    Ok(Open {
        instance: args[0].1.to_string(),
        session_id: args[1].1.to_string(),
        ticket: args[2].1.to_string(),
    })
}
pub fn open(root: &Path, uri: &str) -> Result<Task> {
    let open = parse(uri)?;
    let client = Client::new(root, &open.instance, &open.session_id)?;
    open_with(root, &open, &client)
}
pub fn open_with(root: &Path, open: &Open, client: &Client) -> Result<Task> {
    if client.server != open.instance || client.session_id != open.session_id {
        bail!("设备会话不匹配");
    }
    let id = format!(
        "{:x}",
        Sha256::digest(format!("{}\0{}", client.server, client.session_id))
    );
    let dir = session::task_dir(root, &id)?;
    storage::private_dir(&dir)?;
    // Hold the session lock only while local state changes. The Checkout
    // handoff below performs network calls and must not hold it.
    let task = {
        let _lock = storage::lock(&dir.join("operation.lock"))?;
        if dir.join("task.json").exists() {
            let task = session::load(root, &id)?;
            if task.device_id.as_deref() != Some(&client.device_id) {
                bail!("任务属于另一设备");
            }
            session::local_path(root, &task)?;
            task
        } else {
            claim_and_download(open, client, &dir, &id)?
        }
    };
    if task.mode == "exclusive-edit" {
        // Opening or re-opening retries an unacknowledged Checkout handoff
        // instead of silently continuing without the guarded reservation.
        exclusive::ensure_lease(root, &task.task_id, client)?;
    }
    Ok(task)
}
fn claim_and_download(open: &Open, client: &Client, dir: &Path, id: &str) -> Result<Task> {
    let mut body = client.body();
    body["ticket"] = serde_json::json!(open.ticket);
    client.signed("challenge", "claim", &mut body)?;
    let claimed = client.post("claim", &body)?;
    if claimed["session_id"] != client.session_id || claimed["state"] != "claimed" {
        bail!("服务端未确认会话");
    }
    let mut body = client.body();
    body["ticket"] = serde_json::json!(open.ticket);
    client.signed("read-challenge", "read", &mut body)?;
    body.as_object_mut().unwrap().remove("ticket");
    let grant: Grant = serde_json::from_value(client.post("read-ticket", &body)?)?;
    if !device::uuid(&grant.ticket)
        || grant.expires_in != 60
        || grant.transfer.path != "/seafhttp/cloudfile/read"
        || grant.transfer.method != "GET"
        || grant.transfer.authorization != "Bearer"
        || grant.transfer.redirects
        || grant.transfer.resume
        || !matches!(
            grant.file.mode.as_str(),
            "optimistic-edit" | "exclusive-edit"
        )
        || !device::hex(&grant.file.base_version, 40)
    {
        bail!("下载授权无效或模式不支持；当前仅支持普通本地编辑与独占编辑");
    }
    let ext = &grant.file.extension;
    if ext.len() < 2
        || ext.len() > 17
        || !ext.starts_with('.')
        || !ext[1..].bytes().all(|b| b.is_ascii_alphanumeric())
    {
        bail!("文件扩展名无效");
    }
    let name = format!("CloudFile-{}{}", &client.session_id[..8], ext);
    let target = dir.join(&name);
    if target.exists() {
        bail!("已存在工作副本，请先检查本地文件");
    }
    let mut response = client
        .http
        .get(client.endpoint(&grant.transfer.path))
        .bearer_auth(&grant.ticket)
        .header("Accept-Encoding", "identity")
        .send()
        .map_err(|_| anyhow::anyhow!("下载失败，本地文件未变更"))?;
    let size = response
        .content_length()
        .ok_or_else(|| anyhow::anyhow!("下载缺少文件长度"))?;
    if response.status() != 200
        || size > device::MAX_FILE
        || ["content-encoding", "content-range", "location"]
            .iter()
            .any(|h| response.headers().contains_key(*h))
    {
        bail!("下载响应无效");
    }
    let mut tmp = tempfile::NamedTempFile::new_in(dir)?;
    let n = std::io::copy(&mut (&mut response).take(size + 1), &mut tmp)?;
    if n != size {
        bail!("下载未完成");
    }
    tmp.flush()?;
    tmp.as_file().sync_all()?;
    let hash = session::digest(tmp.path())?;
    tmp.persist_noclobber(target)?;
    let task = Task {
        task_id: id.to_string(),
        device_id: Some(client.device_id.clone()),
        file_name: name,
        mode: grant.file.mode,
        server: client.server.clone(),
        repo_id: String::new(),
        remote_path: String::new(),
        session_id: client.session_id.clone(),
        baseline_file_id: grant.file.base_version,
        baseline_digest: hash,
        size,
    };
    storage::write(&dir.join("task.json"), &task)?;
    Ok(task)
}
