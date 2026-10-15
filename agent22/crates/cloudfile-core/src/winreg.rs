//! 进程内访问 Windows 注册表（仅 Windows 编译）。
//!
//! 移植自 Go 侧 `internal/winreg`。这里原来所有查询/写入都是 spawn `reg.exe`，两个后果：
//!
//! 1. **闪黑窗口**：父进程是 GUI 子系统（Tauri 桌面）或本身没有控制台时，跑一个控制台程序
//!    会让 Windows 分配一个新控制台并一闪而过。Go 侧黑窗口的根因就是每个消息启动了 48 次
//!    `reg.exe`；Rust 侧的 `apps::names()` 逐应用探测、`--status` 查 WebView2、登录项开关
//!    都是同一类调用点。
//! 2. **硬依赖**：机器上禁用/拦掉 `reg.exe`（安全软件、受限环境、沙箱）时**直接失败**，
//!    而不是降级。本机 `cargo test` 有 5 个测试就是这么红的（`could not run reg.exe`）。
//!
//! 现在直接走 advapi32（`winreg` crate），不起进程、不依赖任何外部程序。

use anyhow::{bail, Result};
use winreg::enums::{HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE, KEY_READ, KEY_WRITE};
use winreg::RegKey;

/// 注册表根。调用方继续只维护一个「带 hive 的完整键路径」（例如
/// `HKCU\Software\Google\Chrome\NativeMessagingHosts\<host>`），由 [`split_hive`] 解析，
/// 这样键路径仍然只有一处定义、也仍然可以被跨平台测试钉住。
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Hive {
    CurrentUser,
    LocalMachine,
}

impl Hive {
    fn root(self) -> RegKey {
        match self {
            Hive::CurrentUser => RegKey::predef(HKEY_CURRENT_USER),
            Hive::LocalMachine => RegKey::predef(HKEY_LOCAL_MACHINE),
        }
    }
}

/// 把 `HKCU\Software\...` / `HKLM\Software\...` 拆成 (hive, 子路径)。
pub fn split_hive(key: &str) -> Result<(Hive, String)> {
    for (prefix, hive) in [
        ("HKCU\\", Hive::CurrentUser),
        ("HKLM\\", Hive::LocalMachine),
    ] {
        if let Some(rest) = key.strip_prefix(prefix) {
            if rest.is_empty() {
                bail!("registry key has no path: {key}");
            }
            return Ok((hive, rest.to_string()));
        }
    }
    bail!("unsupported registry hive: {key}")
}

/// 读一个字符串值。键不存在、值不存在、或值的类型不是字符串，都返回 `None`
/// ——调用方一律按「没有」处理，不把「注册表里没这一项」当成错误。
/// `name` 为空串表示默认值（等价于 `reg query <key> /ve`）。
pub fn read_string(key: &str, name: &str) -> Option<String> {
    let (hive, path) = split_hive(key).ok()?;
    let key = hive.root().open_subkey_with_flags(path, KEY_READ).ok()?;
    key.get_value::<String, _>(name).ok()
}

/// 键是否存在。
pub fn key_exists(key: &str) -> bool {
    split_hive(key)
        .is_ok_and(|(hive, path)| hive.root().open_subkey_with_flags(path, KEY_READ).is_ok())
}

/// 写一个字符串值（REG_SZ）。键不存在时一并创建，等价于 `reg add <key> /f`。
pub fn write_string(key: &str, name: &str, data: &str) -> Result<()> {
    let (hive, path) = split_hive(key)?;
    let (key, _) = hive.root().create_subkey(path)?;
    key.set_value(name, &data.to_string())?;
    Ok(())
}

/// 删除一个值。值本来就不在、甚至整个键都不在，都算成功
/// ——调用方要的是「删完之后它不存在」这个终态。
pub fn delete_value(key: &str, name: &str) -> Result<()> {
    let (hive, path) = split_hive(key)?;
    let Ok(key) = hive.root().open_subkey_with_flags(path, KEY_WRITE) else {
        return Ok(());
    };
    match key.delete_value(name) {
        Ok(()) => Ok(()),
        Err(error) if is_missing(&error) => Ok(()),
        Err(error) => Err(error.into()),
    }
}

/// 删除整个键（含子键）。键不存在同样算成功。
pub fn delete_key(key: &str) -> Result<()> {
    let (hive, path) = split_hive(key)?;
    match hive.root().delete_subkey_all(path) {
        Ok(()) => Ok(()),
        Err(error) if is_missing(&error) => Ok(()),
        Err(error) => Err(error.into()),
    }
}

/// `winreg` 把 Win32 错误码原样包成 `io::Error`，`ErrorKind` 往往只是 `Other`，
/// 所以这里同时认错误码 2（ERROR_FILE_NOT_FOUND）。
fn is_missing(error: &std::io::Error) -> bool {
    error.kind() == std::io::ErrorKind::NotFound || error.raw_os_error() == Some(2)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 测试用的一次性键，跑完删掉；不碰任何产品键。
    const SCRATCH: &str = r"HKCU\Software\CloudFileAgent22SelfTest";

    #[test]
    fn hive_prefixes_are_parsed_and_unknown_hives_rejected() {
        assert_eq!(split_hive(r"HKCU\Software\X").unwrap().0, Hive::CurrentUser);
        assert_eq!(
            split_hive(r"HKLM\Software\X").unwrap().0,
            Hive::LocalMachine
        );
        assert_eq!(
            split_hive(r"HKCU\Software\X").unwrap().1,
            r"Software\X".to_string()
        );
        assert!(split_hive(r"HKEY_LOCAL_MACHINE\Software\X").is_err());
        assert!(split_hive("HKCU\\").is_err());
    }

    #[test]
    fn string_values_round_trip_and_missing_reads_are_none() {
        let _ = delete_key(SCRATCH);
        assert!(!key_exists(SCRATCH));
        assert_eq!(read_string(SCRATCH, "name"), None);

        write_string(SCRATCH, "name", "value").unwrap();
        assert!(key_exists(SCRATCH));
        assert_eq!(read_string(SCRATCH, "name").as_deref(), Some("value"));

        // 默认值（空名字）与命名值互不干扰。
        write_string(SCRATCH, "", "default").unwrap();
        assert_eq!(read_string(SCRATCH, "").as_deref(), Some("default"));
        assert_eq!(read_string(SCRATCH, "name").as_deref(), Some("value"));

        delete_value(SCRATCH, "name").unwrap();
        assert_eq!(read_string(SCRATCH, "name"), None);
        assert_eq!(read_string(SCRATCH, "").as_deref(), Some("default"));
        delete_key(SCRATCH).unwrap();
        assert!(!key_exists(SCRATCH));
    }

    #[test]
    fn deleting_what_is_not_there_succeeds() {
        let _ = delete_key(SCRATCH);
        // 「删完之后不存在」就是成功，重复删除不该报错。
        delete_key(SCRATCH).unwrap();
        delete_value(SCRATCH, "name").unwrap();
        assert!(!key_exists(SCRATCH));
    }
}
