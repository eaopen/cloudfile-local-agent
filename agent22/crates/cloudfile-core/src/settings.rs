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
    // 读前加固，对应 Go 的 `internal/taskcenter/preferences.go`（非私有 = ErrCorrupt）：
    // 权限松掉的偏好文件不能当成可信输入。Windows 上无从判断（见 fsaccess::private）。
    crate::fsaccess::private_file(&p)?;
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

/// The explicitly configured workspace root, if the user set one.
///
/// 显式配置时始终优先，并且**不参与搬迁**（镜像就固定在那儿）。返回值只在
/// 配置合法且为绝对路径时给出；文件缺失、损坏或不合法都当作「没配过」，
/// 由 `workspace::resolve` 走平台默认，绝不让一个坏配置把会话卡死。
pub fn configured_workspace_root(root: &Path) -> Option<PathBuf> {
    let settings = load(root).ok()?;
    let configured = settings.workspace_root?;
    let path = PathBuf::from(configured);
    if path.is_absolute() {
        Some(path)
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// An absolute workspace path for the host OS. `Path::is_absolute` is false
    /// for `/tmp/x` on Windows, so a Unix literal here would be rejected by
    /// validation there while passing on macOS.
    #[cfg(windows)]
    const WORKSPACE: &str = "C:/cloudfile/workspace";
    #[cfg(not(windows))]
    const WORKSPACE: &str = "/tmp/cloudfile-workspace";

    fn tmp() -> (tempfile::TempDir, PathBuf) {
        crate::storage::test_root()
    }

    /// 以「Agent 自己会写出的权限」植入一个状态文件。
    ///
    /// `std::fs::write` 得到的是 0644（受 umask 影响），在 POSIX 上会被
    /// `fsaccess::private_file` 判成「不可信状态」—— 那会让下面这些用例变成在测权限，
    /// 而不是在测「损坏的文件怎么处理」。所以显式落成 0600，与 `storage::write` 一致。
    fn plant(path: &Path, bytes: &[u8]) {
        std::fs::write(path, bytes).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
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
        settings.workspace_root = Some(WORKSPACE.into());
        assert!(settings.validate().is_ok());
        settings.schema = 99;
        assert!(settings.validate().is_err());
    }

    #[test]
    fn corrupt_file_is_reported_and_never_reset() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        plant(&path(root), b"{ not json");
        assert!(load(root).is_err());
        // A corrupt file means "never configured" — the workspace root then falls back to
        // the platform default (workspace::resolve) instead of inventing a broken path.
        assert!(configured_workspace_root(root).is_none());
    }

    /// 权限松掉的状态文件必须被拒（fail closed），与 Go 的 preferences.go 同一处判断。
    /// Windows 上这一条无从判断（`fsaccess::private` 恒为 true），所以用例只在 POSIX 上跑。
    #[cfg(unix)]
    #[test]
    fn a_world_readable_preferences_file_is_refused() {
        use std::os::unix::fs::PermissionsExt;
        let (_dir, root) = tmp();
        let root = root.as_path();
        // 先按正常路径写出一份合法设置，确认它本来读得出来。
        update(root, |settings| settings.notify_on_saved = false).unwrap();
        assert!(load(root).is_ok());

        // 只把权限放松（内容一字不改）—— 内容仍然是合法的，所以拒绝的唯一理由就是权限。
        std::fs::set_permissions(&path(root), std::fs::Permissions::from_mode(0o644)).unwrap();
        let error = load(root).unwrap_err().to_string();
        assert!(error.contains("private"), "{error}");
        // 而且「没配过」的回落仍然安全：不因为一份不可信文件就编出一个工作区路径。
        assert!(configured_workspace_root(root).is_none());
    }

    #[test]
    fn saved_settings_round_trip_and_unknown_keys_fail() {
        let (_dir, root) = tmp();
        let root = root.as_path();
        let settings = update(root, |s| {
            s.notify_on_saved = false;
            s.workspace_root = Some(WORKSPACE.into());
        })
        .unwrap();
        assert!(!settings.notify_on_saved);
        assert_eq!(load(root).unwrap(), settings);
        assert_eq!(
            configured_workspace_root(root),
            Some(PathBuf::from(WORKSPACE))
        );
        plant(&path(root), br#"{"schema":1,"upload":true}"#);
        assert!(load(root).is_err());
    }
}
