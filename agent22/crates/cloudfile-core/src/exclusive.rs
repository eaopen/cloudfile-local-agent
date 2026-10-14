//! Exclusive Checkout handoff and manual single-file Commit/Checkin.
//!
//! A device lease is a short-lived, device-signed handoff of the browser's own
//! guarded Checkout. Receiving or signing it authorizes nothing by itself: the
//! server re-evaluates the current actor, ACL, guard and native base file on
//! every step, and the final publication is a separate server-side transaction.
//! This module never takes a file path from a browser and never falls back to
//! ordinary Seafile upload APIs.
use crate::{
    device::{self, Client},
    journal::{self, Action, Intent, Receipt},
    session, storage,
};
use anyhow::{bail, Result};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    io::{Read, Seek, SeekFrom},
    path::Path,
};

/// Same-origin, dedicated guarded route. The bearer belongs in a header.
pub const COMMIT_UPLOAD: &str = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/";

#[derive(Clone, Debug, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Lease {
    pub guard_id: String,
    pub generation: String,
    pub credential_epoch: String,
    pub base_file_id: String,
    /// Checkout proof. Persisted only in the owner-only journal, never logged.
    pub token: String,
}
fn positive(value: &str) -> bool {
    (1..=20).contains(&value.len())
        && value
            .bytes()
            .next()
            .is_some_and(|b| (b'1'..=b'9').contains(&b))
        && value.bytes().all(|b| b.is_ascii_digit())
}
pub fn valid(lease: &Lease) -> bool {
    device::uuid(&lease.guard_id)
        && positive(&lease.generation)
        && positive(&lease.credential_epoch)
        && device::hex(&lease.base_file_id, 40)
        && device::hex(&lease.token, 64)
}
fn action(value: &Action) -> &'static str {
    match value {
        Action::Commit => "commit",
        Action::Checkin => "checkin",
        Action::OptimisticUpdate => "optimistic-update",
    }
}

/// Retryable until the local journal has been durably written and confirmed.
pub fn receive_lease(client: &Client) -> Result<Lease> {
    let mut body = client.body();
    client.signed("lease-challenge", "lease", &mut body)?;
    let value = client.post("lease-receive", &body)?;
    if value["session_id"] != client.session_id {
        bail!("设备租约会话不匹配");
    }
    let lease: Lease = serde_json::from_value(value["lease"].clone())?;
    if !valid(&lease) {
        bail!("设备租约格式无效");
    }
    Ok(lease)
}
/// Must only run after the lease is durably journaled. A failed call keeps it.
pub fn confirm_lease(client: &Client) -> Result<()> {
    let mut body = client.body();
    client.signed("lease-confirm-challenge", "lease", &mut body)?;
    let value = client.post("lease-confirm", &body)?;
    if value["confirmed"] != true || value["session_id"] != client.session_id {
        bail!("Checkout 交付确认未通过");
    }
    Ok(())
}
/// Extends only this device's own current reservation, inside the hard deadline.
pub fn renew_lease(client: &Client) -> Result<()> {
    let mut body = client.body();
    client.signed("lease-renew-challenge", "lease", &mut body)?;
    let value = client.post("lease-renew", &body)?;
    if value["renewed"] != true || value["session_id"] != client.session_id {
        bail!("Checkout 续租未确认");
    }
    Ok(())
}
/// Explicit user action. The server refuses while a CommitIntent is pending.
pub fn abandon_lease(client: &Client, lease: &Lease) -> Result<()> {
    if !valid(lease) {
        bail!("租约无效，无法释放 Checkout");
    }
    let mut body = client.body();
    body["guard_id"] = json!(lease.guard_id);
    body["generation"] = json!(lease.generation);
    body["credential_epoch"] = json!(lease.credential_epoch);
    body["base_file_id"] = json!(lease.base_file_id);
    body["token"] = json!(lease.token);
    client.signed("lease-abandon-challenge", "lease", &mut body)?;
    let value = client.post("lease-abandon", &body)?;
    if value["session_id"] != client.session_id || value["released"] != true {
        bail!("释放 Checkout 未确认");
    }
    Ok(())
}
pub(crate) fn request(client: &Client, lease: &Lease, intent: &Intent) -> Result<Value> {
    if !valid(lease)
        || !matches!(intent.action, Action::Commit | Action::Checkin)
        || !device::hex(&intent.content_digest, 64)
        || !device::hex(&intent.base_file_id, 40)
        || intent.snapshot_size > device::MAX_FILE
        || lease.base_file_id != intent.base_file_id
    {
        bail!("受保护的不可变单文件提交快照无效");
    }
    Ok(
        json!({"session_id":client.session_id,"device_id":client.device_id,
        "intent_id":intent.intent_id,"guard_id":lease.guard_id,
        "generation":lease.generation,"credential_epoch":lease.credential_epoch,
        "token":lease.token,"base_file_id":intent.base_file_id,
        "content_digest":intent.content_digest,"snapshot_size":intent.snapshot_size,
        "action":action(&intent.action)}),
    )
}
/// Persists the intent server-side without authorizing a single byte of upload.
pub fn prepare(client: &Client, lease: &Lease, intent: &Intent) -> Result<()> {
    let mut body = request(client, lease, intent)?;
    client.signed("commit-challenge", "commit", &mut body)?;
    let value = client.post("commit-authorize", &body)?;
    if value["intent_id"] != intent.intent_id.to_string()
        || value["state"] != "prepared"
        || value["device_authorized"] != true
        || value["publication"] != "not_started"
    {
        bail!("服务端未确认提交意图");
    }
    Ok(())
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Transfer {
    path: String,
    method: String,
    authorization: String,
    redirects: bool,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Grant {
    ticket: String,
    expires_in: u64,
    intent_id: uuid::Uuid,
    content_digest: String,
    snapshot_size: u64,
    native_device_publish: bool,
    transfer: Transfer,
}
fn decode(value: &Value, intent: &Intent) -> Result<Receipt> {
    let file = value["result_file_id"].as_str().unwrap_or("");
    let expected = value["expected_file_id"].as_str().unwrap_or("");
    if value["intent_id"] != intent.intent_id.to_string()
        || value["state"] != "published"
        || value["content_digest"].as_str() != Some(intent.content_digest.as_str())
        || expected != intent.base_file_id
        || value["action"].as_str() != Some(action(&intent.action))
        || value["checked_in"] != (intent.action == Action::Checkin)
        || !device::hex(file, 40)
    {
        bail!("服务端提交回执与原意图不匹配");
    }
    Ok(Receipt {
        intent_id: intent.intent_id,
        state: "published".into(),
        action: intent.action.clone(),
        base_file_id: expected.into(),
        content_digest: intent.content_digest.clone(),
        result_file_id: file.into(),
        checked_in: intent.action == Action::Checkin,
    })
}
/// Sends the frozen original snapshot exactly once. Never retries by itself.
pub fn publish(
    root: &Path,
    id: &str,
    client: &Client,
    lease: &Lease,
    intent: &Intent,
) -> Result<Receipt> {
    // A large snapshot may reuse native CDC blocks instead of resending bytes.
    // `None` means nothing was published, so the proven full transfer below
    // stays available with the same prepared intent.
    if let Some(receipt) = crate::cdc::publish(
        root,
        id,
        client,
        intent,
        crate::cdc::Mode::Commit,
        Some(lease),
    )? {
        return Ok(receipt);
    }
    let mut body = request(client, lease, intent)?;
    let dir = session::task_dir(root, id)?;
    let source = dir.join(format!("{}.snapshot", intent.intent_id));
    if !std::fs::symlink_metadata(&source)?.is_file() {
        bail!("原提交快照不可用");
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
        let read = frozen.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        hash.update(&buffer[..read]);
    }
    if n != intent.snapshot_size || format!("{:x}", hash.finalize()) != intent.content_digest {
        bail!("原提交快照已变化，已停止发送");
    }
    frozen.seek(SeekFrom::Start(0))?;
    // `commit-authorize` already consumed the prepare nonce: sending bytes needs
    // its own single-use proof, never a replay of the earlier one.
    client.signed("commit-challenge", "commit", &mut body)?;
    let grant: Grant = serde_json::from_value(client.post("commit-upload-authorize", &body)?)?;
    if grant.intent_id != intent.intent_id
        || grant.content_digest != intent.content_digest
        || grant.snapshot_size != intent.snapshot_size
        || !grant.native_device_publish
        || !(1..=60).contains(&grant.expires_in)
        || grant.transfer.path != COMMIT_UPLOAD
        || grant.transfer.method != "POST"
        || grant.transfer.authorization != "Bearer"
        || grant.transfer.redirects
        || !grant.ticket.starts_with("v1.")
        || !(43..=14000).contains(&grant.ticket.len())
        || !grant.ticket[3..]
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    {
        bail!("提交上传授权与原文件不匹配");
    }
    let response = client
        .http
        .post(client.endpoint(COMMIT_UPLOAD))
        .header("Content-Type", "application/octet-stream")
        .header("Accept", "application/json")
        .header("Idempotency-Key", intent.intent_id.to_string())
        .header("Content-Length", intent.snapshot_size)
        .bearer_auth(grant.ticket)
        .body(reqwest::blocking::Body::sized(frozen, intent.snapshot_size))
        .send()
        .map_err(|_| anyhow::anyhow!("提交响应中断；本地文件已保留，请查询原提交结果"))?;
    decode(&device::decode(response)?["intent"], intent)
}
/// Only queries. `None` means the original intent is not published yet.
pub fn query(client: &Client, intent: &Intent) -> Result<Option<Receipt>> {
    if !matches!(intent.action, Action::Commit | Action::Checkin) {
        bail!("仅支持提交意图查询");
    }
    let mut body = json!({"session_id":client.session_id,"device_id":client.device_id,
        "intent_id":intent.intent_id});
    client.signed("commit-status-challenge", "commit", &mut body)?;
    let value = client.post("commit-status", &body)?;
    let found = &value["intent"];
    if found["intent_id"] != intent.intent_id.to_string() {
        bail!("提交查询不匹配");
    }
    if found["state"] == "not-found" || found["state"] == "prepared" {
        if found["checked_in"] == true || !found["result_file_id"].as_str().unwrap_or("").is_empty()
        {
            bail!("未发布的提交结果声称已成功");
        }
        return Ok(None);
    }
    Ok(Some(decode(found, intent)?))
}
fn bound(root: &Path, id: &str, client: &Client) -> Result<(session::Task, Lease)> {
    let task = session::load(root, id)?;
    if task.mode != "exclusive-edit" || task.device_id.is_none() {
        bail!("此任务不是独占编辑会话");
    }
    if task.server != client.server || task.session_id != client.session_id {
        bail!("设备会话不匹配");
    }
    if task.device_id.as_ref() != Some(&client.device_id) {
        bail!("此任务的设备配对已变更");
    }
    if journal::closed(root, id)? {
        bail!("本次独占编辑已签入并释放，请从网页重新发起");
    }
    let lease =
        journal::lease(root, id)?.ok_or_else(|| anyhow::anyhow!("租约尚未就绪，请先重新领取"))?;
    if !valid(&lease) {
        bail!("本地租约无效，请从网页重新发起");
    }
    Ok((task, lease))
}
/// Production entry point: resolves the OS-protected paired device key.
fn exclusive(root: &Path, id: &str) -> Result<(session::Task, Client, Lease)> {
    let task = session::load(root, id)?;
    if task.mode != "exclusive-edit" || task.device_id.is_none() {
        bail!("此任务不是独占编辑会话");
    }
    let client = Client::new(root, &task.server, &task.session_id)?;
    let (task, lease) = bound(root, id, &client)?;
    Ok((task, client, lease))
}
/// Explicit user action. An existing pending intent must be recovered first.
pub fn commit(root: &Path, id: &str, checkin: bool) -> Result<Value> {
    let (_, client, _) = exclusive(root, id)?;
    commit_with(root, id, &client, checkin)
}
/// Injectable-device variant used by protocol tests.
pub fn commit_with(root: &Path, id: &str, client: &Client, checkin: bool) -> Result<Value> {
    let (task, lease) = bound(root, id, client)?;
    let dir = session::task_dir(root, id)?;
    let _lock = storage::lock(&dir.join("upload.lock"))?;
    if journal::pending(root, id)?.is_some() {
        return Ok(json!({"state":"pending"}));
    }
    if !checkin && session::digest(&session::local_path(root, &task)?)? == task.baseline_digest {
        return Ok(json!({"state":"unchanged"}));
    }
    let action = if checkin {
        Action::Checkin
    } else {
        Action::Commit
    };
    let intent = journal::capture(root, id, action)?;
    prepare(client, &lease, &intent)?;
    let receipt = publish(root, id, client, &lease, &intent)?;
    journal::accept(root, id, receipt)?;
    Ok(json!({"state":"published","checked_in":checkin}))
}
/// Recovers the original intent without creating a new one or replacing bytes.
pub fn recover(root: &Path, id: &str) -> Result<Value> {
    let (_, client, _) = exclusive(root, id)?;
    recover_with(root, id, &client)
}
pub fn recover_with(root: &Path, id: &str, client: &Client) -> Result<Value> {
    let (_, lease) = bound(root, id, client)?;
    let dir = session::task_dir(root, id)?;
    let _lock = storage::lock(&dir.join("upload.lock"))?;
    let intent = journal::pending(root, id)?.ok_or_else(|| anyhow::anyhow!("没有待确认的提交"))?;
    if let Some(receipt) = query(client, &intent)? {
        journal::accept(root, id, receipt)?;
        return Ok(json!({"state":"published"}));
    }
    prepare(client, &lease, &intent)?;
    let receipt = publish(root, id, client, &lease, &intent)?;
    journal::accept(root, id, receipt)?;
    Ok(json!({"state":"published"}))
}
/// Releases the Checkout and marks the local session closed. Never automatic.
pub fn release(root: &Path, id: &str) -> Result<Value> {
    let (_, client, _) = exclusive(root, id)?;
    release_with(root, id, &client)
}
pub fn release_with(root: &Path, id: &str, client: &Client) -> Result<Value> {
    let (_, lease) = bound(root, id, client)?;
    let dir = session::task_dir(root, id)?;
    let _lock = storage::lock(&dir.join("upload.lock"))?;
    if journal::pending(root, id)?.is_some() {
        bail!("请先查询并处理原提交，再释放 Checkout");
    }
    abandon_lease(client, &lease)?;
    journal::close(root, id)?;
    Ok(json!({"state":"released"}))
}
pub fn renew_checkout(root: &Path, id: &str) -> Result<Value> {
    let (_, client, _) = exclusive(root, id)?;
    renew_with(root, id, &client)
}
pub fn renew_with(root: &Path, id: &str, client: &Client) -> Result<Value> {
    bound(root, id, client)?;
    renew_lease(client)?;
    Ok(json!({"state":"renewed"}))
}
/// Durable handoff used when a session is opened or re-opened.
pub fn ensure_lease(root: &Path, id: &str, client: &Client) -> Result<Lease> {
    let lease = match journal::lease(root, id)? {
        Some(lease) if valid(&lease) => lease,
        Some(_) => bail!("本地租约损坏，请从网页重新发起独占编辑"),
        None => {
            // The server keeps the handoff retriable until it is acknowledged.
            let lease = receive_lease(client)?;
            journal::set_lease(root, id, &lease)?;
            lease
        }
    };
    if !journal::lease_acknowledged(root, id)? {
        confirm_lease(client)?;
        journal::acknowledge_lease(root, id)?;
    }
    Ok(lease)
}
