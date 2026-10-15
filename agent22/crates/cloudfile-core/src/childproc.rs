//! 子进程控制台窗口控制。
//!
//! 移植自 Go 侧 `internal/childproc`。Windows 上父进程是 GUI 子系统
//! （Tauri 桌面、`cloudfile-native-host` 被 Chrome 以无控制台方式拉起）时，
//! 启动一个**控制台**子程序会让系统给它分配一个新控制台窗口——用户看到的就是
//! 一闪而过的黑框。`CREATE_NO_WINDOW` 才是真正消掉它的标志（`SW_HIDE` 不行：
//! 那只是把新建的窗口隐藏，而且会连带影响子进程自己创建的窗口）。
//!
//! 其它平台是空操作，所以调用点不需要 `cfg` 分支。

use std::process::Command;

/// Windows 的 `CREATE_NO_WINDOW`。
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// 让这个子进程不弹控制台窗口。
///
/// 只用于「我们只要它的输出」的程序（探测版本、注册表/系统查询等）。
/// **不要**用在要打开文档的应用上：那类调用本来就需要窗口（`apps::open`）。
pub fn hide(command: &mut Command) -> &mut Command {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(CREATE_NO_WINDOW);
    }
    command
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hide_is_usable_in_a_builder_chain() {
        let mut command = Command::new("cmd");
        let returned = hide(command.arg("/c").arg("exit 0"));
        assert_eq!(returned.get_args().count(), 2);
    }
}
