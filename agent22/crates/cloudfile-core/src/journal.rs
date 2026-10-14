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
/// This primitive does not authorize upload or acquire checkout. Never expose it to browsers.
pub fn capture(root: &Path, id: &str, action: Action) -> Result<Intent> {
    let task = session::load(root, id)?;
    if !matches!(task.mode.as_str(), "local-edit" | "optimistic-edit") {
        bail!("editable task required");
    }
    let dir = session::task_dir(root, id)?;
    let _guard = storage::lock(&dir.join("operation.lock"))?;
    let mut j = load(&dir)?;
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
        bail!("working copy changed during capture");
    }
    if session::digest(&path)? != format!("{:x}", hash.clone().finalize()) {
        bail!("文件仍在保存，请稍后重试");
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
    task.baseline_file_id = receipt.result_file_id;
    task.baseline_digest = i.content_digest.clone();
    task.size = i.snapshot_size;
    storage::write(&dir.join("task.json"), &task)?;
    j.pending = None;
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
