//! 「启动」文件夹里的快捷方式（`.lnk`），仅 Windows 编译。
//!
//! 这是登录项的第二条途径：`HKCU\...\Run` 被组策略或安全软件锁死时（企业机器上
//! 很常见），往「启动」文件夹放一个快捷方式效果等价、要求更低。Go 侧对应的实现是
//! `register-agent2-windows.ps1` 里 `Set-Autostart` 的 `WScript.Shell.CreateShortcut`。
//!
//! 写 `.lnk` 走 shell 自己的 COM 接口（`IShellLinkW` + `IPersistFile`），与
//! `CreateShortcut` 是同一套东西：格式由 shell 维护，不用自己拼二进制。代价是多一个
//! 依赖（`windows` crate），换来的是不会随 Windows 版本变动的文件格式。
//!
//! 这里刻意只做「写 / 读目标 / 删」三件事，不判断业务语义：
//! 什么时候算「我们的快捷方式」由 `autostart` 决定。

use anyhow::{bail, Context, Result};
use std::path::{Path, PathBuf};
use windows::core::{Interface, PCWSTR};
use windows::Win32::System::Com::{
    CoCreateInstance, CoInitializeEx, CoTaskMemFree, CoUninitialize, IPersistFile,
    CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED, STGM_READ,
};
use windows::Win32::UI::Shell::{
    FOLDERID_Startup, IShellLinkW, SHGetKnownFolderPath, ShellLink, KNOWN_FOLDER_FLAG,
};

/// `RPC_E_CHANGED_MODE`（`0x80010106`）：本线程已按另一种套间模型初始化过 COM。
/// 写死字面量而不是引常量，免得随 crate 的模块改名而失效 —— 它是个固定的 HRESULT。
const RPC_E_CHANGED_MODE: i32 = 0x8001_0106u32 as i32;

/// `GetPath` 的输出缓冲。`MAX_PATH` 不够（长路径可达 32k），但 shell 自己写出的
/// 目标路径不可能超过 4096 —— 这是注册表 / 快捷方式层的实际上限。
const TARGET_CAPACITY: usize = 4096;

/// 一次 COM 使用期。写 / 读都要先 `CoInitializeEx`，否则 `ShellLink` 会拒绝创建
/// （`CO_E_NOTINITIALIZED`）。只有我们自己初始化成功（`S_OK` / `S_FALSE`）时才收尾：
/// `RPC_E_CHANGED_MODE` 说明另一个套间模型已经占着这个线程，我们无权 `CoUninitialize`。
struct Com {
    owned: bool,
}

impl Com {
    fn initialize() -> Result<Self> {
        // `CoInitializeEx` 在这里返回原始 `HRESULT`（不是 `Result`），要自己判。
        let result = unsafe { CoInitializeEx(None, COINIT_APARTMENTTHREADED) };
        if result.is_ok() {
            // S_OK（首次初始化）与 S_FALSE（本线程已按同一模型初始化过，引用计数 +1）
            // 都归我们收尾，两者语义上都是「这次调用成功」。
            return Ok(Com { owned: true });
        }
        if result.0 == RPC_E_CHANGED_MODE {
            return Ok(Com { owned: false });
        }
        Err(anyhow::anyhow!(
            "COM could not be initialised for the shell link: 0x{:08X}",
            result.0 as u32
        ))
    }
}

impl Drop for Com {
    fn drop(&mut self) {
        if self.owned {
            unsafe { CoUninitialize() };
        }
    }
}

/// 转成以 NUL 结尾的宽字符串。内嵌 NUL 会让 Win32 把路径截断，直接拒掉。
fn wide(value: &str) -> Result<Vec<u16>> {
    if value.is_empty() {
        bail!("shell link text must not be empty");
    }
    if value.contains('\0') {
        bail!("shell link text must not contain a NUL");
    }
    Ok(value.encode_utf16().chain(std::iter::once(0)).collect())
}

/// 「启动」文件夹。由 shell 决定而不是拼 `%APPDATA%`：企业机器上它可能被组策略
/// 重定向到网络位置（漫游配置文件），那种情况下拼出来的路径是错的。
pub fn startup_dir() -> Option<PathBuf> {
    if let Some(path) = known_folder_startup() {
        return Some(path);
    }
    // shell 调用不可用时（COM 被锁 / 极简环境）退到文档化的默认位置。
    let roaming = std::env::var_os("APPDATA").map(PathBuf::from)?;
    Some(default_startup_dir(&roaming))
}

/// `%APPDATA%` 下的标准路径。纯函数，跨平台测试可以钉住它（Windows 之外的
/// 「启动」文件夹不存在，这个函数只在 Windows 被调用）。
pub fn default_startup_dir(roaming_app_data: &Path) -> PathBuf {
    roaming_app_data
        .join("Microsoft")
        .join("Windows")
        .join("Start Menu")
        .join("Programs")
        .join("Startup")
}

fn known_folder_startup() -> Option<PathBuf> {
    let _com = Com::initialize().ok()?;
    // `SHGetKnownFolderPath` 自己分配返回字符串，用 `CoTaskMemFree` 还回去。
    let raw =
        unsafe { SHGetKnownFolderPath(&FOLDERID_Startup, KNOWN_FOLDER_FLAG(0), None) }.ok()?;
    let value = unsafe { raw.to_string() }.ok();
    unsafe { CoTaskMemFree(Some(raw.0 as *const core::ffi::c_void)) };
    value.map(PathBuf::from).filter(|path| path.is_absolute())
}

/// 写一个指向 `program` 的快捷方式（覆盖同名文件）。`arguments` 会拼成
/// 快捷方式的命令行参数 —— 登录项靠它把 `--autostart` 传给桌面进程。
pub fn write(
    link: &Path,
    program: &Path,
    arguments: &[String],
    working_directory: &Path,
    description: &str,
) -> Result<()> {
    let _com = Com::initialize()?;
    let shell: IShellLinkW = unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER) }
        .context("the Windows shell link service is unavailable")?;
    let target = wide(&program.to_string_lossy())?;
    let directory = wide(&working_directory.to_string_lossy())?;
    let text = wide(description)?;
    let joined = arguments.join(" ");
    // SAFETY: 每个宽字符串都在本函数栈上活到调用之后，且都以 NUL 结尾。
    unsafe {
        shell.SetPath(PCWSTR(target.as_ptr()))?;
        if !joined.is_empty() {
            shell.SetArguments(PCWSTR(wide(&joined)?.as_ptr()))?;
        }
        shell.SetWorkingDirectory(PCWSTR(directory.as_ptr()))?;
        shell.SetDescription(PCWSTR(text.as_ptr()))?;
    }
    let persist: IPersistFile = shell.cast()?;
    let path = wide(&link.to_string_lossy())?;
    // `true` = 记住这个位置（下次 `Save` 不带路径也能写回去）。
    unsafe { persist.Save(PCWSTR(path.as_ptr()), true) }
        .with_context(|| format!("the shortcut could not be written: {}", link.display()))?;
    Ok(())
}

/// 读快捷方式当前指向的目标。读不出来（文件不在 / 不是 `.lnk` / shell 拒绝）
/// 一律返回 `None`：调用方据此判断「这个快捷方式不归我们管」，绝不删掉看不准的东西。
pub fn target(link: &Path) -> Option<PathBuf> {
    let _com = Com::initialize().ok()?;
    let shell: IShellLinkW =
        unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER) }.ok()?;
    let persist: IPersistFile = shell.cast().ok()?;
    let path = wide(&link.to_string_lossy()).ok()?;
    unsafe { persist.Load(PCWSTR(path.as_ptr()), STGM_READ) }.ok()?;
    let mut buffer = [0u16; TARGET_CAPACITY];
    // 空指针 = 不关心目标文件的属性；`0` = 不额外解析环境变量（原样返回）。
    unsafe { shell.GetPath(&mut buffer, std::ptr::null_mut(), 0) }.ok()?;
    let end = buffer.iter().position(|unit| *unit == 0)?;
    let value = String::from_utf16_lossy(&buffer[..end]);
    (!value.is_empty()).then(|| PathBuf::from(value))
}

/// 删掉快捷方式。本来就不在算成功 —— 调用方要的是「删完之后它不存在」。
pub fn remove(link: &Path) -> Result<()> {
    match std::fs::remove_file(link) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error).context("the shortcut could not be removed"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn startup_directory_matches_the_documented_location() {
        let path = default_startup_dir(Path::new("C:/Users/x/AppData/Roaming"));
        assert_eq!(
            path,
            Path::new("C:/Users/x/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup")
        );
        // 必须是「漫游 AppData」下的这一串，不能是 LocalAppData 或盘根。
        assert!(path.ends_with("Startup"));
    }

    #[test]
    fn shell_link_text_rejects_what_would_truncate() {
        assert_eq!(wide("a").unwrap(), vec![b'a' as u16, 0]);
        assert!(wide("").is_err());
        assert!(wide("a\0b").is_err());
    }

    /// 真实的 COM 往返：建一个临时目录里的 `.lnk`，读回目标，再删掉。
    /// 需要 Windows shell，因此默认 `#[ignore]`（`cargo test -- --ignored` 跑）。
    #[test]
    #[ignore = "needs the Windows shell (COM)"]
    fn shortcut_round_trip_against_the_real_shell() {
        let directory = tempfile::tempdir().unwrap();
        let link = directory.path().join("CloudFileAgent22.lnk");
        let program = Path::new("C:\\Windows\\System32\\notepad.exe");
        let arguments = vec!["--autostart".to_string()];

        write(&link, program, &arguments, Path::new("C:\\Windows"), "自测").unwrap();
        assert!(link.is_file(), "shortcut was not created");
        // 目标是可执行文件，shell 会把路径规范化后原样还回来。
        let read = target(&link).expect("target unreadable");
        assert_eq!(
            read.file_name().unwrap().to_string_lossy().to_lowercase(),
            "notepad.exe"
        );

        remove(&link).unwrap();
        assert!(!link.exists());
        // 删不在的东西也算成功。
        remove(&link).unwrap();
        // 读一个不是快捷方式的文件：返回 None 而不是报错。
        let plain = directory.path().join("plain.txt");
        std::fs::write(&plain, b"not a link").unwrap();
        assert!(target(&plain).is_none());
    }
}
