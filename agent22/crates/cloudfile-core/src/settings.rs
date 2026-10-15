//! Local user preferences for the task centre.
//!
//! These options are presentation and lifecycle choices only. They never
//! authorise an upload, extend a Checkout lease or release a lock; the durable
//! journal remains the single source of truth for anything that touches the
//! server. A corrupt file must therefore be reported, never silently reset.
use crate::storage;
use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

pub const SCHEMA: u32 = 1;
const MAX_POSITIONS: usize = 16;
const MAX_DISPLAY: usize = 128;
const COORDINATE: i32 = 32_768;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FloatPosition {
    /// Stable monitor identity, so each display keeps its own resting place.
    pub display: String,
    pub x: i32,
    pub y: i32,
}

impl FloatPosition {
    fn validate(&self) -> Result<()> {
        if self.display.len() > MAX_DISPLAY || self.display.chars().any(char::is_control) {
            bail!("invalid display identity");
        }
        if self.x.abs() > COORDINATE || self.y.abs() > COORDINATE {
            bail!("float position out of range");
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Settings {
    pub schema: u32,
    /// Lightweight toast after a working copy is saved locally.
    pub notify_on_saved: bool,
    /// Escalated notice when a working copy needs review.
    pub notify_on_review: bool,
    /// Draggable resident entry. Never an authorisation, only a shortcut.
    pub float_enabled: bool,
    /// Hide the entry while no local task exists, as the reference builds do.
    pub float_hidden_when_empty: bool,
    /// Advisory mirror of the login-item state; the OS remains authoritative.
    pub autostart: bool,
    /// Directory that holds working copies. `None` means the private agent root.
    pub workspace_root: Option<String>,
    /// Explicit local openers, evaluated before detection and the OS default.
    pub open_rules: Vec<crate::apps::OpenRule>,
    /// Per-display resting positions for the dragging entry.
    pub float_positions: BTreeMap<String, FloatPosition>,
}

impl Default for Settings {
    fn default() -> Self {
        Settings {
            schema: SCHEMA,
            notify_on_saved: true,
            notify_on_review: true,
            float_enabled: true,
            float_hidden_when_empty: true,
            autostart: false,
            workspace_root: None,
            open_rules: Vec::new(),
            float_positions: BTreeMap::new(),
        }
    }
}

impl Settings {
    pub fn validate(&self) -> Result<()> {
        if self.schema != SCHEMA {
            bail!("unsupported settings schema; preserve the file for review");
        }
        if let Some(root) = &self.workspace_root {
            let path = Path::new(root);
            if root.is_empty() || root.len() > 4096 || root.contains('\0') || !path.is_absolute() {
                bail!("workspace root must be an absolute path");
            }
        }
        if self.float_positions.len() > MAX_POSITIONS {
            bail!("too many saved float positions");
        }
        for position in self.float_positions.values() {
            position.validate()?;
        }
        crate::apps::validate_rules(&self.open_rules)?;
        Ok(())
    }
}

pub fn path(root: &Path) -> PathBuf {
    root.join("settings.json")
}

/// Missing means "never configured", which is the documented default set.
pub fn load(root: &Path) -> Result<Settings> {
    let p = path(root);
    if !p.exists() {
        return Ok(Settings::default());
    }
    let settings: Settings = storage::read(&p)?;
    settings.validate()?;
    Ok(settings)
}

pub fn save(root: &Path, settings: &Settings) -> Result<()> {
    settings.validate()?;
    let _lock = storage::lock(&root.join("settings.lock"))?;
    storage::write(&path(root), settings)
}

/// Applies a partial update without exposing the whole struct to the caller.
pub fn update(root: &Path, change: impl FnOnce(&mut Settings)) -> Result<Settings> {
    let _lock = storage::lock(&root.join("settings.lock"))?;
    let mut settings = load(root)?;
    change(&mut settings);
    settings.validate()?;
    storage::write(&path(root), &settings)?;
    Ok(settings)
}

pub fn position(root: &Path, display: &str) -> Result<Option<FloatPosition>> {
    Ok(load(root)?.float_positions.get(display).cloned())
}

/// Records where the user left the entry on one display. Advisory only: the
/// caller must still clamp the restored point into the current screen bounds.
pub fn set_position(root: &Path, display: &str, x: i32, y: i32) -> Result<()> {
    let position = FloatPosition {
        display: display.to_string(),
        x,
        y,
    };
    position.validate()?;
    let key = display.to_string();
    update(root, |settings| {
        settings.float_positions.insert(key, position);
    })?;
    Ok(())
}

/// The directory working copies live in. Configuration cannot escape the store
/// silently: a missing or unusable override falls back to the agent root.
pub fn workspace_root(root: &Path) -> PathBuf {
    match load(root) {
        Ok(settings) => settings
            .workspace_root
            .map(PathBuf::from)
            .unwrap_or_else(|| root.join("tasks")),
        Err(_) => root.join("tasks"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp() -> (tempfile::TempDir, PathBuf) {
        crate::storage::test_root()
    }

    #[test]
    fn defaults_never_authorise_anything() {
        let settings = Settings::default();
        settings.validate().unwrap();
        assert!(settings.notify_on_saved && settings.notify_on_review);
        assert!(settings.float_enabled && settings.float_hidden_when_empty);
        assert!(!settings.autostart);
        assert!(settings.workspace_root.is_none());
    }

    #[test]
    fn positions_are_kept_per_display() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        set_position(root, "mac:Built-in Retina Display", 120, 340).unwrap();
        set_position(root, "win:\\\\.\\DISPLAY2", -40, 8).unwrap();
        assert_eq!(
            position(root, "mac:Built-in Retina Display")
                .unwrap()
                .unwrap()
                .x,
            120
        );
        let other = position(root, "win:\\\\.\\DISPLAY2").unwrap().unwrap();
        assert_eq!((other.x, other.y), (-40, 8));
        assert!(position(root, "mac:missing").unwrap().is_none());
    }

    #[test]
    fn rejects_unsafe_overrides_and_positions() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        assert!(set_position(root, "d", 999_999, 0).is_err());
        assert!(set_position(root, "d", 0, 999_999).is_err());
        let mut settings = Settings {
            workspace_root: Some("relative/path".into()),
            ..Default::default()
        };
        assert!(settings.validate().is_err());
        settings.workspace_root = Some("/tmp/ok".into());
        assert!(settings.validate().is_ok());
        settings.schema = 99;
        assert!(settings.validate().is_err());
    }

    #[test]
    fn corrupt_file_is_reported_and_never_reset() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        std::fs::write(path(root), b"{ not json").unwrap();
        assert!(load(root).is_err());
        // The fallback keeps working copies reachable instead of inventing a new root.
        assert_eq!(workspace_root(root), root.join("tasks"));
    }

    #[test]
    fn saved_settings_round_trip_and_unknown_keys_fail() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        let settings = update(root, |s| {
            s.notify_on_saved = false;
            s.workspace_root = Some("/tmp/cloudfile-workspace".into());
        })
        .unwrap();
        assert!(!settings.notify_on_saved);
        assert_eq!(load(root).unwrap(), settings);
        assert_eq!(
            workspace_root(root),
            PathBuf::from("/tmp/cloudfile-workspace")
        );
        std::fs::write(path(root), br#"{"schema":1,"upload":true}"#).unwrap();
        assert!(load(root).is_err());
    }
}
