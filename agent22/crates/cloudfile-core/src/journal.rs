//! Durable publication intent. Network adapters may only accept a matching receipt.
use crate::{session, storage};
use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    fs,
    io::{Read, Write},
    path::Path,
};
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Action {
    OptimisticUpdate,
    Commit,
    Checkin,
}
#[derive(Debug)]
pub struct WorkingCopyChanged;
impl std::fmt::Display for WorkingCopyChanged {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("文件仍在保存，请稍后重试")
    }
}
impl std::error::Error for WorkingCopyChanged {}
#[derive(Clone, Serialize, Deserialize)]
pub struct Intent {
    pub intent_id: uuid::Uuid,
    pub action: Action,
    pub base_file_id: String,
    pub content_digest: String,
    pub snapshot_size: u64,
}
#[derive(Serialize, Deserialize)]
pub struct Journal {
    pub schema: u32,
    pub pending: Option<Intent>,
    pub receipt: Option<Receipt>,
    /// Exclusive Checkout handoff. The token is a Checkout proof: it must only
    /// ever live in this owner-only file and is erased once the session closes.
    #[serde(default)]
    pub lease: Option<crate::exclusive::Lease>,
    /// The device handoff was acknowledged, so the server erased its ciphertext.
    /// A lost acknowledgement must be retried, never assumed.
    #[serde(default)]
    pub lease_ack: bool,
    /// A confirmed checkin released the server Checkout; the local session is
    /// finished and must not accept further commits or a new reservation.
    #[serde(default)]
    pub closed: bool,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Receipt {
    pub intent_id: uuid::Uuid,
    pub state: String,
    pub action: Action,
    pub base_file_id: String,
    pub content_digest: String,
    pub result_file_id: String,
    pub checked_in: bool,
}
fn load(dir: &Path) -> Result<Journal> {
    let p = dir.join("journal.json");
    if !p.exists() {
        return Ok(Journal {
            schema: 1,
            pending: None,
            receipt: None,
            lease: None,
            lease_ack: false,
            closed: false,
        });
    }
    let j: Journal = storage::read(&p)?;
    if j.schema != 1 {
        bail!("unsupported journal; preserve for recovery");
    }
    Ok(j)
}
pub fn pending(root: &Path, id: &str) -> Result<Option<Intent>> {
    Ok(load(&session::task_dir(root, id)?)?.pending)
}
pub fn lease(root: &Path, id: &str) -> Result<Option<crate::exclusive::Lease>> {
    Ok(load(&session::task_dir(root, id)?)?.lease)
}
pub fn closed(root: &Path, id: &str) -> Result<bool> {
    Ok(load(&session::task_dir(root, id)?)?.closed)
}
pub fn lease_acknowledged(root: &Path, id: &str) -> Result<bool> {
    Ok(load(&session::task_dir(root, id)?)?.lease_ack)
}
/// Records the durable acknowledgement of a device Checkout handoff.
pub fn acknowledge_lease(root: &Path, id: &str) -> Result<()> {
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
    if j.lease.is_none() {
        bail!("no durable checkout lease");
    }
    j.lease_ack = true;
    storage::write(&dir.join("journal.json"), &j)
}
/// Durably stores the device Checkout handoff before it is acknowledged.
pub fn set_lease(root: &Path, id: &str, lease: &crate::exclusive::Lease) -> Result<()> {
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
    if j.closed {
        bail!("checkout already closed");
    }
    if j.lease.as_ref().is_some_and(|current| current != lease) {
        bail!("existing checkout lease differs; manual reconciliation required");
    }
    j.lease = Some(lease.clone());
    storage::write(&dir.join("journal.json"), &j)
}
pub fn close(root: &Path, id: &str) -> Result<()> {
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
    if j.pending.is_some() {
        bail!("recover original pending intent first");
    }
    j.closed = true;
    j.lease = None;
    j.lease_ack = false;
    storage::write(&dir.join("journal.json"), &j)
}
/// This primitive does not authorize upload or acquire checkout. Never expose it to browsers.
pub fn capture(root: &Path, id: &str, action: Action) -> Result<Intent> {
    capture_observed(root, id, action, None)
}
pub fn capture_observed(
    root: &Path,
    id: &str,
    action: Action,
    expected: Option<&str>,
) -> Result<Intent> {
    let task = session::load(root, id)?;
    if !matches!(
        task.mode.as_str(),
        "local-edit" | "optimistic-edit" | "exclusive-edit"
    ) {
        bail!("editable task required");
    }
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
    if j.closed {
        bail!("checkout already closed");
    }
    if j.pending.is_some() {
        bail!("recover original pending intent first");
    }
    let path = session::local_path(root, &task)?;
    let mut input = fs::File::open(&path)?;
    let before = input.metadata()?;
    if before.len() > crate::device::MAX_FILE {
        bail!("snapshot exceeds limit");
    }
    let mut snapshot = tempfile::NamedTempFile::new_in(&dir)?;
    let mut hash = Sha256::new();
    let mut size = 0u64;
    let mut buffer = [0; 65536];
    loop {
        let n = input.read(&mut buffer)?;
        if n == 0 {
            break;
        }
        size += n as u64;
        if size > crate::device::MAX_FILE {
            bail!("snapshot exceeds limit");
        }
        hash.update(&buffer[..n]);
        snapshot.write_all(&buffer[..n])?;
    }
    let after = input.metadata()?;
    if size != before.len()
        || before.modified()? != after.modified()?
        || before.len() != after.len()
    {
        return Err(WorkingCopyChanged.into());
    }
    let captured_digest = format!("{:x}", hash.clone().finalize());
    if session::digest(&path)? != captured_digest
        || expected.is_some_and(|value| value != captured_digest)
    {
        return Err(WorkingCopyChanged.into());
    }
    snapshot.as_file().sync_all()?;
    let intent = Intent {
        intent_id: uuid::Uuid::new_v4(),
        action,
        base_file_id: task.baseline_file_id,
        content_digest: format!("{:x}", hash.finalize()),
        snapshot_size: size,
    };
    snapshot.persist_noclobber(dir.join(format!("{}.snapshot", intent.intent_id)))?;
    j.pending = Some(intent.clone());
    storage::write(&dir.join("journal.json"), &j)?;
    Ok(intent)
}
pub fn accept(root: &Path, id: &str, receipt: Receipt) -> Result<()> {
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
    let i = j
        .pending
        .as_ref()
        .ok_or_else(|| anyhow::anyhow!("no pending intent"))?;
    if receipt.intent_id != i.intent_id
        || receipt.state != "published"
        || receipt.action != i.action
        || receipt.base_file_id != i.base_file_id
        || receipt.content_digest != i.content_digest
        || receipt.checked_in != (i.action == Action::Checkin)
        || receipt.result_file_id.len() != 40
        || !receipt
            .result_file_id
            .bytes()
            .all(|x| x.is_ascii_hexdigit())
    {
        bail!("publication receipt does not match original intent");
    }
    // Persist receipt first. A crash before task metadata update must retain original intent.
    j.receipt = Some(receipt.clone());
    storage::write(&dir.join("journal.json"), &j)?;
    let mut task = session::load(root, id)?;
    task.baseline_file_id = receipt.result_file_id.clone();
    task.baseline_digest = i.content_digest.clone();
    task.size = i.snapshot_size;
    storage::write(&dir.join("task.json"), &task)?;
    let checked_in = i.action == Action::Checkin;
    j.pending = None;
    if checked_in {
        // The server released the guarded Checkout; keep only a closed marker.
        j.closed = true;
        j.lease = None;
        j.lease_ack = false;
    } else if let Some(lease) = j.lease.as_mut() {
        lease.base_file_id = receipt.result_file_id;
    }
    storage::write(&dir.join("journal.json"), &j)?;
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn lost_response_keeps_intent_and_rejects_unbound_receipt() {
        let tmp = tempfile::tempdir().unwrap();
        let canonical = tmp.path().canonicalize().unwrap();
        let root = canonical.as_path();
        let id = "a".repeat(64);
        let dir = session::task_dir(root, &id).unwrap();
        storage::private_dir(&dir).unwrap();
        fs::write(dir.join("a.txt"), b"original").unwrap();
        let t = session::Task {
            task_id: id.clone(),
            device_id: None,
            file_name: "a.txt".into(),
            mode: "local-edit".into(),
            server: "https://example.com".into(),
            repo_id: String::new(),
            remote_path: "/a.txt".into(),
            session_id: String::new(),
            baseline_file_id: "b".repeat(40),
            baseline_digest: String::new(),
            size: 8,
        };
        storage::write(&dir.join("task.json"), &t).unwrap();
        let i = capture(root, &id, Action::Commit).unwrap();
        fs::write(dir.join("a.txt"), b"new local changes").unwrap();
        assert_eq!(pending(root, &id).unwrap().unwrap().intent_id, i.intent_id);
        assert!(capture(root, &id, Action::Commit).is_err());
        assert_eq!(
            fs::read(dir.join(format!("{}.snapshot", i.intent_id))).unwrap(),
            b"original"
        );
        let mut r = Receipt {
            intent_id: uuid::Uuid::new_v4(),
            state: "published".into(),
            action: Action::Commit,
            base_file_id: i.base_file_id.clone(),
            content_digest: i.content_digest.clone(),
            result_file_id: "c".repeat(40),
            checked_in: false,
        };
        assert!(accept(root, &id, r.clone()).is_err());
        r.intent_id = i.intent_id;
        accept(root, &id, r).unwrap();
        assert!(pending(root, &id).unwrap().is_none());
        assert_eq!(fs::read(dir.join("a.txt")).unwrap(), b"new local changes");
    }
}
