//! 「这个文件/目录是不是只有属主能碰」—— 读侧加固的唯一出处。
//!
//! 与 Go 的 `internal/fsaccess` 一一对应，**包括 Windows 上是刻意的 no-op** 这一点，
//! 原因写在 [`private`] 上。存在这个模块的意义是：判据只有一处定义，调用方不必各自
//! 猜「什么算私有」，也就不会出现「某个读点忘了加固」。
use anyhow::{bail, Result};
use std::fs;
use std::path::Path;

/// POSIX 权限位是否「只有属主可访问」：组与其他位必须全为 0（文件 0600 / 目录 0700）。
///
/// 单独抽成纯函数是为了**可测**：完整的 [`private`] 在 Windows 上恒为 true，
/// 而这里能在任何平台上跑真值表，于是 unix 侧的逻辑不必依赖一台 macOS 才能验证。
pub fn private_mode(mode: u32) -> bool {
    mode & 0o077 == 0
}

/// 元数据是否表示「只有属主可访问」。
///
/// **Windows 上恒为 true，这是刻意的**（与 Go 侧同一处判断，理由照抄过来）：
/// Windows 没有 POSIX 权限位——`Metadata::permissions()` 只反映只读属性，
/// 组/其他的位是合成出来的，套 POSIX 判据会把**每一个**任务目录、日志与会话记录
/// 都判成「不安全」，等于把整个本地任务层静默关掉。真实的访问控制由 NTFS ACL 管，
/// 而 ACL 不通过 `Metadata` 暴露。
///
/// 所以 Windows 上剩下的加固一条都不少（绝对路径、非重解析点、普通文件、体积上限），
/// 只是「权限私有」这一条在 Windows 上无从判断。**不要「顺手」把它改成返回 false**：
/// 那不会更安全，只会让 Windows 上的 Agent 读不到自己的状态。
pub fn private(meta: &fs::Metadata) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        private_mode(meta.permissions().mode())
    }
    #[cfg(not(unix))]
    {
        let _ = meta;
        true
    }
}

/// 读前加固的组合：必须是**普通文件**且私有。
///
/// 用 `symlink_metadata`，所以符号链接一律不算普通文件 —— 与 [`crate::storage::read`]
/// 同一策略：被加固的是一条路径不能指向别处。
pub fn private_file(path: &Path) -> Result<fs::Metadata> {
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_file() || !private(&meta) {
        bail!("state file is not a private regular file");
    }
    Ok(meta)
}

/// 目录版：必须是**真目录**（不是重解析点）且私有。
pub fn private_directory(path: &Path) -> Result<fs::Metadata> {
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_dir() || !private(&meta) {
        bail!("state directory is not a private directory");
    }
    Ok(meta)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 真值表在任何平台都能跑 —— 这是本模块在 Windows 上仍被覆盖的方式。
    #[test]
    fn only_owner_bits_may_be_set() {
        assert!(private_mode(0o600));
        assert!(private_mode(0o700));
        assert!(private_mode(0o000));
        // 只读位、属主位之外的任何一位都不行。
        assert!(!private_mode(0o640));
        assert!(!private_mode(0o604));
        assert!(!private_mode(0o644));
        assert!(!private_mode(0o750));
        assert!(!private_mode(0o701));
        // 高位（setuid/setgid/sticky、文件类型位）不参与判断，只看低 9 位里的组与其他。
        assert!(private_mode(0o100_700));
        assert!(!private_mode(0o100_770));
    }

    #[cfg(unix)]
    #[test]
    fn posix_metadata_is_judged_by_its_mode() {
        use std::os::unix::fs::PermissionsExt;
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.json");
        std::fs::write(&path, b"{}").unwrap();

        std::fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        assert!(private_file(&path).is_ok());

        std::fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(private_file(&path).is_err());

        // 目录同样按位判断。
        assert!(private_directory(directory.path()).is_ok()); // tempfile 建的是 0700
        std::fs::set_permissions(directory.path(), fs::Permissions::from_mode(0o755)).unwrap();
        assert!(private_directory(directory.path()).is_err());
    }

    /// 符号链接不是普通文件 —— 与 `storage::read` 一致，读一处的加固不能比另一处松。
    #[cfg(unix)]
    #[test]
    fn a_symlink_is_never_a_private_file() {
        let directory = tempfile::tempdir().unwrap();
        let real = directory.path().join("real.json");
        std::fs::write(&real, b"{}").unwrap();
        std::fs::set_permissions(&real, fs::Permissions::from_mode(0o600)).unwrap();
        let link = directory.path().join("link.json");
        std::os::unix::fs::symlink(&real, &link).unwrap();
        // 链接自身是 0777，而且即便它指向一个 0600 的文件，也不该被当成私有状态文件。
        assert!(private_file(&link).is_err());
    }

    /// 非普通文件（这里是目录）不能当状态文件用。
    #[test]
    fn a_directory_is_not_a_file() {
        let directory = tempfile::tempdir().unwrap();
        assert!(private_file(directory.path()).is_err());
        let path = directory.path().join("plain");
        std::fs::write(&path, b"x").unwrap();
        assert!(private_directory(&path).is_err());
    }

    /// Windows 上判据是 no-op —— 把这件事**钉住**：它是设计，不是遗漏。
    /// 谁要是为了「更安全」改成返回 false，Windows 上的 Agent 会立刻读不到自己的状态，
    /// 这条测试会先红。
    #[test]
    fn on_windows_the_check_is_deliberately_a_no_op() {
        let directory = tempfile::tempdir().unwrap();
        assert!(private(&std::fs::metadata(directory.path()).unwrap()));
        #[cfg(windows)]
        {
            // 连一个明确「不私有」的路径也必须是 true —— 因为 Windows 上这个判据
            // 根本无从判断（真实访问由 NTFS ACL 管）。
            assert!(private_file(&directory.path().join("missing")).is_err()); // 不存在仍然报错
        }
    }
}
