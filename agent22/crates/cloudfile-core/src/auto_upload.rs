//! A small polling observer over Agent-owned working copies only.
//! Existing intents are never sent automatically; uncertain results pause durably.
use crate::{journal, session, storage, upload};
use anyhow::Result;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{collections::HashMap, path::Path};

const SETTLE_MS: u64 = 3_000;

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub enabled: bool,
    pub pause_reason: Option<String>,
    pub last_synced_at: Option<u64>,
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            enabled: true,
            pause_reason: None,
            last_synced_at: None,
        }
    }
}
pub fn settings(root: &Path, id: &str) -> Result<Settings> {
    let path = session::task_dir(root, id)?.join("auto-upload.json");
    if path.exists() {
        storage::read(&path)
    } else {
        Ok(Settings::default())
    }
}
fn update(root: &Path, id: &str, change: impl FnOnce(&mut Settings)) -> Result<()> {
    let dir = session::task_dir(root, id)?;
    let _lock = storage::lock(&dir.join("auto-upload.lock"))?;
    let mut current = settings(root, id)?;
    change(&mut current);
    storage::write(&dir.join("auto-upload.json"), &current)
}
pub fn set_enabled(root: &Path, id: &str, enabled: bool) -> Result<()> {
    let task = session::load(root, id)?;
    if task.mode != "optimistic-edit" || task.device_id.is_none() {
        anyhow::bail!("此任务不支持自动上传");
    }
    update(root, id, |current| {
        current.enabled = enabled;
        if enabled {
            current.pause_reason = None;
        }
    })
}
pub fn confirmed(root: &Path, id: &str) -> Result<()> {
    update(root, id, |current| {
        current.pause_reason = None;
        current.last_synced_at = Some(crate::now());
    })
}
fn pause(root: &Path, id: &str, error: &str) -> Result<()> {
    update(root, id, |current| {
        current.pause_reason = Some(error.chars().take(400).collect());
    })
}
struct Observation {
    digest: String,
    since: u64,
}
#[derive(Default)]
pub struct Observer {
    observations: HashMap<String, Observation>,
}
impl Observer {
    pub fn scan(&mut self, root: &Path, monotonic_ms: u64) -> Result<()> {
        self.scan_with(root, monotonic_ms, |id, digest| {
            upload::save_observed(root, id, digest)
        })
    }
    pub fn scan_with(
        &mut self,
        root: &Path,
        monotonic_ms: u64,
        mut save: impl FnMut(&str, &str) -> Result<Value>,
    ) -> Result<()> {
        let tasks = session::tasks(root)?;
        self.observations
            .retain(|id, _| tasks.iter().any(|t| &t.task_id == id));
        for task in tasks {
            let id = &task.task_id;
            let current = settings(root, id)?;
            if task.mode != "optimistic-edit"
                || task.device_id.is_none()
                || !current.enabled
                || current.pause_reason.is_some()
                || journal::pending(root, id)?.is_some()
            {
                self.observations.remove(id);
                continue;
            }
            // Office commonly replaces the file atomically. Temporary missing
            // or unreadable copies reset the settling window, not the baseline.
            let path = match session::local_path(root, &task) {
                Ok(path) => path,
                Err(_) => {
                    self.observations.remove(id);
                    continue;
                }
            };
            if std::fs::metadata(&path)?.len() > crate::device::MAX_FILE {
                pause(root, id, "文件超过 128 MiB，自动上传已暂停；本地文件已保留")?;
                self.observations.remove(id);
                continue;
            }
            let digest = match session::digest(&path) {
                Ok(digest) => digest,
                Err(_) => {
                    self.observations.remove(id);
                    continue;
                }
            };
            if digest == task.baseline_digest {
                self.observations.remove(id);
                continue;
            }
            let observation = self
                .observations
                .entry(id.clone())
                .or_insert_with(|| Observation {
                    digest: digest.clone(),
                    since: monotonic_ms,
                });
            if observation.digest != digest {
                *observation = Observation {
                    digest,
                    since: monotonic_ms,
                };
                continue;
            }
            if monotonic_ms.saturating_sub(observation.since) < SETTLE_MS {
                continue;
            }
            self.observations.remove(id);
            // upload::save_observed acquires the shared upload lock, rechecks
            // pending state, and binds capture to the exact observed digest.
            match save(id, &digest) {
                Ok(result) if result["state"] == "published" => confirmed(root, id)?,
                Ok(result)
                    if ["unchanged", "changing", "busy", "pending"]
                        .iter()
                        .any(|state| result["state"] == *state) => {}
                Ok(_) => pause(root, id, "上传结果未确认，自动上传已暂停；请查询原上传结果")?,
                Err(error) => pause(root, id, &error.to_string())?,
            }
        }
        Ok(())
    }
}
