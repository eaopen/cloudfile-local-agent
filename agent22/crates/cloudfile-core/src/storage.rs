use anyhow::{bail, Context, Result};
use serde::{de::DeserializeOwned, Serialize};
use std::{
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
};

/// 本机私有目录（Agent 的整棵树：配置、任务、版本）。
///
/// `CLOUDFILE_AGENT2_HOME` 可以覆盖它 —— 对齐 Go 侧 `install.cmd` 与 `internal/home`：
/// 现场常把 Agent 装到别的盘，安装器与测试也需要能把整棵树搬走。
pub fn root() -> Result<PathBuf> {
    let path = match std::env::var_os("CLOUDFILE_AGENT2_HOME") {
        Some(home) if !home.is_empty() => {
            let path = PathBuf::from(home);
            if !path.is_absolute() {
                bail!("CLOUDFILE_AGENT2_HOME must be an absolute path");
            }
            path
        }
        _ => dirs::data_local_dir()
            .context("user data directory unavailable")?
            .join("CloudFileAgent22"),
    };
    private_dir(&path)?;
    Ok(path)
}
pub fn private_dir(path: &Path) -> Result<()> {
    // Reject symlink ancestors before following paths in the private store.
    for part in path.ancestors() {
        if let Ok(meta) = fs::symlink_metadata(part) {
            if meta.file_type().is_symlink() {
                bail!("symlink in private storage");
            }
        }
    }
    fs::create_dir_all(path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}
pub fn write<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    let parent = path.parent().context("missing parent")?;
    private_dir(parent)?;
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    serde_json::to_writer(&mut tmp, value)?;
    tmp.write_all(b"\n")?;
    tmp.as_file().sync_all()?;
    tmp.persist(path)?;
    #[cfg(unix)]
    fs::File::open(parent)?.sync_all()?;
    Ok(())
}
/// Atomic byte write for payloads that are already serialized (for example a
/// manifest copied verbatim during a rollback).
pub fn write_bytes(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path.parent().context("missing parent")?;
    private_dir(parent)?;
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    tmp.write_all(bytes)?;
    tmp.as_file().sync_all()?;
    tmp.persist(path)?;
    #[cfg(unix)]
    fs::File::open(parent)?.sync_all()?;
    Ok(())
}
pub fn read<T: DeserializeOwned>(path: &Path) -> Result<T> {
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_file() || meta.len() > 1024 * 1024 {
        bail!("invalid state file");
    }
    let mut bytes = Vec::new();
    fs::File::open(path)?
        .take(1024 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 1024 * 1024 {
        bail!("state file too large");
    }
    Ok(serde_json::from_slice(&bytes)?)
}
pub fn lock(path: &Path) -> Result<fs::File> {
    private_dir(path.parent().context("missing lock parent")?)?;
    if fs::symlink_metadata(path).is_ok_and(|m| !m.is_file()) {
        bail!("invalid lock");
    }
    let file = fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path)?;
    fs2::FileExt::try_lock_exclusive(&file).context("another operation is active")?;
    Ok(file)
}

/// Test-only root. The private store rejects symlinked ancestors, and the
/// platform temporary directory is itself reached through one on macOS, so the
/// canonical path is what the tests must use.
#[cfg(test)]
pub fn test_root() -> (tempfile::TempDir, PathBuf) {
    let directory = tempfile::tempdir().unwrap();
    let root = directory.path().canonicalize().unwrap();
    (directory, root)
}
