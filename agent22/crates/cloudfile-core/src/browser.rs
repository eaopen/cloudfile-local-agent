//! 浏览器侧的两件小事：找到 Chrome、把用户带到「扩展管理页」。
//!
//! 引导页的「安装扩展」那一步只能**提示**，不能替用户装：Chrome 不允许程序
//! 静默安装非商店扩展。能自动做的是「把页面打开、把目录指出来」，剩下的
//! 「开发者模式 → 加载已解压的扩展程序」必须由用户点。
//!
//! 因此这里只做两件低风险的事：
//! 1. [`chrome_path`]：按注册表 App Paths 和常见安装位置找 `chrome.exe`；
//! 2. [`open_extensions_page`]：用它打开 `chrome://extensions`（找不到就退回默认浏览器）。
//!
//! 不写注册表、不改 Chrome 配置、不传任何命令行开关给扩展 —— 打开一个页面而已。

use anyhow::{Context, Result};
use std::path::{Path, PathBuf};

/// Chrome 扩展管理页。
pub const EXTENSIONS_URL: &str = "chrome://extensions/";

/// 找到本机的 `chrome.exe`。
///
/// 顺序：`HKCU` App Paths → `HKLM` App Paths → 常见安装目录。
/// 用 App Paths 而不是遍历磁盘：那是 Chrome 安装程序自己写的权威位置，
/// 也顺便覆盖了用户装在非默认目录的情况。
#[cfg(windows)]
pub fn chrome_path() -> Option<PathBuf> {
    app_path_entries()
        .into_iter()
        .find(|path| path.is_file())
        .or_else(|| common_paths().into_iter().find(|path| path.is_file()))
}

#[cfg(not(windows))]
pub fn chrome_path() -> Option<PathBuf> {
    None
}

#[cfg(windows)]
fn app_path_entries() -> Vec<PathBuf> {
    use winreg::enums::{HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE, KEY_READ};
    use winreg::RegKey;

    const KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe";
    let mut found = Vec::new();
    for hive in [HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE] {
        let root = RegKey::predef(hive);
        if let Ok(key) = root.open_subkey_with_flags(KEY, KEY_READ) {
            if let Ok(value) = key.get_value::<String, _>("") {
                if !value.trim().is_empty() {
                    found.push(PathBuf::from(value));
                }
            }
        }
    }
    found
}

#[cfg(windows)]
fn common_paths() -> Vec<PathBuf> {
    let mut roots = Vec::new();
    if let Ok(program_files) = std::env::var("ProgramFiles") {
        roots.push(PathBuf::from(program_files).join(r"Google\Chrome\Application\chrome.exe"));
    }
    if let Ok(program_files) = std::env::var("ProgramFiles(x86)") {
        roots.push(PathBuf::from(program_files).join(r"Google\Chrome\Application\chrome.exe"));
    }
    if let Ok(local) = std::env::var("LOCALAPPDATA") {
        roots.push(PathBuf::from(local).join(r"Google\Chrome\Application\chrome.exe"));
    }
    roots
}

/// 打开扩展管理页。返回实际用的浏览器路径（`None` 表示交给了系统默认程序）。
pub fn open_extensions_page() -> Result<Option<PathBuf>> {
    if let Some(chrome) = chrome_path() {
        std::process::Command::new(&chrome)
            .arg(EXTENSIONS_URL)
            .spawn()
            .with_context(|| format!("无法启动 {}", chrome.display()))?;
        return Ok(Some(chrome));
    }
    // 没有 Chrome（或没装在标准位置）：把 URL 交给系统默认浏览器。
    // Chrome 专有协议在别的浏览器里打不开，但至少不会静默什么都不发生。
    open::that(EXTENSIONS_URL).context("无法打开扩展管理页；请在浏览器地址栏输入 chrome://extensions")?;
    Ok(None)
}

/// 在资源管理器里打开一个目录（扩展解压目录）。
pub fn open_directory(directory: &Path) -> Result<()> {
    open::that(directory).with_context(|| format!("无法打开目录 {}", directory.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_extensions_page_is_the_canonical_chrome_url() {
        // 尾斜杠不是装饰：Chrome 对 `chrome://extensions` 与 `chrome://extensions/`
        // 都会跳转，但命令行启动时用规范形式能少一次重定向。
        assert_eq!(EXTENSIONS_URL, "chrome://extensions/");
    }

    #[test]
    fn a_missing_chrome_never_panics_the_lookup() {
        // 找不到只能是 None，不能 panic —— 引导页要能继续把步骤说明打出来。
        let found = chrome_path();
        if let Some(path) = found {
            assert!(path.is_absolute(), "{path:?}");
        }
    }
}
