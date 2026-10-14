//! Full-file upload only. The task chooses the working copy; browsers never supply a path.
use crate::{
    device::{self, Client},
    journal::{self, Action, Intent, Receipt},
    session::{self, Task},
    storage,
};
use anyhow::{bail, Result};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    io::{Read, Seek, SeekFrom},
    path::Path,
};
const UPLOAD: &str = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/";
#[derive(Deserialize)]
struct Transfer {
    path: String,
    method: String,
    authorization: String,
    redirects: bool,
}
#[derive(Deserialize)]
struct Grant {
    ticket: String,
    expires_in: u64,
    intent_id: uuid::Uuid,
    content_digest: String,
    snapshot_size: u64,
    native_device_publish: bool,
    transfer: Transfer,
}
fn task_client(root: &Path, id: &str) -> Result<(Task, Client)> {
    let t = session::load(root, id)?;
    if t.mode != "optimistic-edit" || t.device_id.is_none() {
        bail!("此任务不具备 Agent 上传授权，请从网页重新选择普通本地编辑");
    }
    let c = Client::new(root, &t.server, &t.session_id)?;
    if Some(&c.device_id) != t.device_id.as_ref() {
        bail!("此任务的设备配对已变更");
    }
    Ok((t, c))
}
pub fn save(root: &Path, id: &str) -> Result<Value> {
    let (_, client) = task_client(root, id)?;
    save_with(root, id, &client)
}
pub fn recover(root: &Path, id: &str) -> Result<Value> {
    let (_, client) = task_client(root, id)?;
    let dir = session::task_dir(root, id)?;
    let _lock = storage::lock(&dir.join("upload.lock"))?;
    let intent = journal::pending(root, id)?.ok_or_else(|| anyhow::anyhow!("没有待确认上传"))?;
    if let Some(receipt) = query(&client, &intent)? {
        journal::accept(root, id, receipt)?;
        Ok(json!({"state":"published"}))
    } else {
        Ok(json!({"state":"pending"}))
    }
}
pub fn save_with(root: &Path, id: &str, client: &Client) -> Result<Value> {
    save_inner(root, id, client, None)
}
pub fn save_observed(root: &Path, id: &str, digest: &str) -> Result<Value> {
    let (_, client) = task_client(root, id)?;
    save_observed_with(root, id, &client, digest)
}
pub fn save_observed_with(root: &Path, id: &str, client: &Client, digest: &str) -> Result<Value> {
    match save_inner(root, id, client, Some(digest)) {
        Err(error) if error.is::<journal::WorkingCopyChanged>() => Ok(json!({"state":"changing"})),
        result => result,
    }
}
fn save_inner(root: &Path, id: &str, client: &Client, observed: Option<&str>) -> Result<Value> {
    let dir = session::task_dir(root, id)?;
    let _lock = match storage::lock(&dir.join("upload.lock")) {
        Ok(lock) => lock,
        Err(_) if observed.is_some() => return Ok(json!({"state":"busy"})),
        Err(_) => bail!("此任务正在上传，请等待结果"),
    };
    let task = session::load(root, id)?;
    if task.mode != "optimistic-edit"
        || task.server != client.server
        || task.session_id != client.session_id
        || task.device_id.as_ref() != Some(&client.device_id)
    {
        bail!("设备上传任务不匹配");
    }
    let intent = if let Some(i) = journal::pending(root, id)? {
        // Background observation never retries an existing intent. A manual
        // operation may have captured it after the observer last checked.
        if observed.is_some() {
            return Ok(json!({"state":"pending"}));
        }
        if let Some(receipt) = query(client, &i)? {
            journal::accept(root, id, receipt)?;
            return Ok(json!({"state":"published"}));
        }
        i
    } else {
        if session::digest(&session::local_path(root, &task)?)? == task.baseline_digest {
            return Ok(json!({"state":"unchanged"}));
        }
        journal::capture_observed(root, id, Action::OptimisticUpdate, observed)?
    };
    let receipt = publish(root, id, client, &intent)?;
    journal::accept(root, id, receipt)?;
    Ok(json!({"state":"published"}))
}
fn receipt(value: &Value, intent: &Intent) -> Result<Receipt> {
    let file = value["result_file_id"].as_str().unwrap_or("");
    if value["intent_id"] != intent.intent_id.to_string()
        || value["state"] != "published"
        || value["base_file_id"] != intent.base_file_id
        || !device::hex(file, 40)
    {
        bail!("上传结果尚未确认，请查询原上传结果");
    }
    Ok(Receipt {
        intent_id: intent.intent_id,
        state: "published".into(),
        action: Action::OptimisticUpdate,
        base_file_id: intent.base_file_id.clone(),
        content_digest: intent.content_digest.clone(),
        result_file_id: file.into(),
        checked_in: false,
    })
}
pub fn query(client: &Client, intent: &Intent) -> Result<Option<Receipt>> {
    let mut body = client.body();
    body["intent_id"] = json!(intent.intent_id);
    client.signed("optimistic-status-challenge", "commit", &mut body)?;
    let value = client.post("optimistic-status", &body)?;
    let found = &value["intent"];
    if found["intent_id"] != intent.intent_id.to_string() {
        bail!("上传查询不匹配");
    }
    if found["state"] == "not-found" {
        return Ok(None);
    }
    if found["base_file_id"] != intent.base_file_id
        || found["content_digest"] != intent.content_digest
        || found["snapshot_size"] != intent.snapshot_size
    {
        bail!("原上传快照与服务器记录不匹配");
    }
    if found["state"] == "prepared" && found["result_file_id"].as_str().unwrap_or("").is_empty() {
        return Ok(None);
    }
    Ok(Some(receipt(found, intent)?))
}
fn publish(root: &Path, id: &str, client: &Client, intent: &Intent) -> Result<Receipt> {
    if intent.action != Action::OptimisticUpdate
        || intent.snapshot_size > device::MAX_FILE
        || !device::hex(&intent.base_file_id, 40)
        || !device::hex(&intent.content_digest, 64)
    {
        bail!("无效的原上传快照");
    }
    let dir = session::task_dir(root, id)?;
    let source = dir.join(format!("{}.snapshot", intent.intent_id));
    if !std::fs::symlink_metadata(&source)?.is_file() {
        bail!("原上传快照不可用");
    }
    let mut input = std::fs::File::open(source)?;
    let mut frozen = tempfile::tempfile_in(&dir)?;
    let n = std::io::copy(&mut (&mut input).take(device::MAX_FILE + 1), &mut frozen)?;
    frozen.sync_all()?;
    frozen.seek(SeekFrom::Start(0))?;
    use sha2::{Digest, Sha256};
    let mut hash = Sha256::new();
    let mut buffer = [0; 65536];
    loop {
        let n = frozen.read(&mut buffer)?;
        if n == 0 {
            break;
        }
        hash.update(&buffer[..n]);
    }
    if n != intent.snapshot_size || format!("{:x}", hash.finalize()) != intent.content_digest {
        bail!("原上传快照已变化，已停止发送");
    }
    frozen.seek(SeekFrom::Start(0))?;
    let mut body = client.body();
    body["intent_id"] = json!(intent.intent_id);
    body["base_file_id"] = json!(intent.base_file_id);
    body["content_digest"] = json!(intent.content_digest);
    body["snapshot_size"] = json!(intent.snapshot_size);
    client.signed("optimistic-upload-challenge", "commit", &mut body)?;
    let grant: Grant = serde_json::from_value(client.post("optimistic-upload-authorize", &body)?)?;
    if grant.intent_id != intent.intent_id
        || grant.content_digest != intent.content_digest
        || grant.snapshot_size != intent.snapshot_size
        || !grant.native_device_publish
        || !(1..=60).contains(&grant.expires_in)
        || grant.transfer.path != UPLOAD
        || grant.transfer.method != "POST"
        || grant.transfer.authorization != "Bearer"
        || grant.transfer.redirects
        || grant.ticket.len() < 43
        || grant.ticket.len() > 14000
        || !grant.ticket.starts_with("v1.")
        || !grant.ticket[3..]
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    {
        bail!("上传授权与原文件不匹配");
    }
    let response = client
        .http
        .post(client.endpoint(UPLOAD))
        .header("Content-Type", "application/octet-stream")
        .header("Idempotency-Key", intent.intent_id.to_string())
        .header("Content-Length", intent.snapshot_size)
        .bearer_auth(grant.ticket)
        .body(reqwest::blocking::Body::sized(frozen, intent.snapshot_size))
        .send()
        .map_err(|_| anyhow::anyhow!("上传响应中断；本地文件已保留，请查询上传结果"))?;
    receipt(&device::decode(response)?, intent)
}
