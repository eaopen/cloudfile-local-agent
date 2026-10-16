//! Optional login item for the resident desktop process.
//!
//! Starting at login must never be required for correctness: the agent works
//! when launched by hand, and a refused or impossible login item is reported
//! rather than silently ignored. macOS uses a per-user LaunchAgent so nothing
//! needs administrator rights.
//!
//! Windows has **two** ways in, and the shipped product used both: the
//! current-user `Run` key first, and — when that is locked down by group policy
//! or security software, which is common on managed machines — a shortcut in the
//! user's `Startup` folder. Both carry [`LOGIN_ARGUMENT`], because the desktop
//! has to come up in the tray instead of stealing focus at every login.
use crate::settings;
use anyhow::{bail, Result};
use std::path::{Path, PathBuf};

#[cfg(target_os = "macos")]
use crate::storage;
// Both login-item back ends report a failed spawn through `context`, so the
// trait must be in scope on either platform, not only macOS.
#[cfg(any(target_os = "macos", windows))]
use anyhow::Context;

pub const MACOS_LABEL: &str = "com.cloudfile.agent22.autostart";
pub const WINDOWS_VALUE: &str = "CloudFileAgent22";
/// 兜底途径：「启动」文件夹里的快捷方式名。名字只属于本安装，删除时据此认领。
pub const WINDOWS_LINK: &str = "CloudFileAgent22.lnk";
/// 快捷方式描述（资源管理器里鼠标划过时显示）。
pub const WINDOWS_DESCRIPTION: &str = "CloudFile 任务中心（托盘）";
/// 登录项传给桌面进程的参数。两个平台共用：macOS 写进 LaunchAgent 的
/// `ProgramArguments`，Windows 写进 Run 键的值和快捷方式参数。
/// 少了它，每次登录都会弹出一个抢焦点的窗口 —— `desktop --autostart` 的语义正是
/// 「进托盘、不开窗」。
pub const LOGIN_ARGUMENT: &str = "--autostart";

/// 登录项实际落在哪里。`enabled()` 只说「有没有」，这个说「在哪一条途径上」——
/// 两者一起才够回答「为什么托盘的开关是开的，但设置界面说没设」。
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LoginItem {
    /// `HKCU\...\Run` 里的一个值（Windows 首选）。
    RunKey,
    /// 「启动」文件夹里的 `.lnk`（Windows 在 Run 键被锁时的兜底）。
    StartupFolder,
    /// macOS 的 per-user LaunchAgent。
    LaunchAgent,
}

impl LoginItem {
    /// 写进 JSON / 命令行输出的稳定名字（界面据此显示原因，不要随意改）。
    pub fn id(self) -> &'static str {
        match self {
            LoginItem::RunKey => "run-key",
            LoginItem::StartupFolder => "startup-folder",
            LoginItem::LaunchAgent => "launch-agent",
        }
    }
}

fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

/// LaunchAgent property list. Kept hand-written so the agent needs no plist
/// library; the shape is fixed and covered by tests.
pub fn plist(label: &str, program: &Path, arguments: &[String]) -> Result<String> {
    if label.is_empty()
        || !label
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '.' || c == '-' || c == '_')
    {
        bail!("invalid login item label");
    }
    if !program.is_absolute() {
        bail!("login item program must be an absolute path");
    }
    let mut items = vec![program.to_string_lossy().into_owned()];
    items.extend(arguments.iter().cloned());
    let rendered: String = items
        .iter()
        .map(|item| format!("    <string>{}</string>\n", xml_escape(item)))
        .collect();
    Ok(format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
         <!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n\
         <plist version=\"1.0\">\n\
         <dict>\n\
         \x20 <key>Label</key>\n\
         \x20 <string>{label}</string>\n\
         \x20 <key>ProgramArguments</key>\n\
         \x20 <array>\n{rendered}\x20 </array>\n\
         \x20 <key>RunAtLoad</key>\n\
         \x20 <true/>\n\
         \x20 <key>KeepAlive</key>\n\
         \x20 <dict>\n\
         \x20   <key>SuccessfulExit</key>\n\
         \x20   <false/>\n\
         \x20 </dict>\n\
         \x20 <key>LimitLoadToSessionType</key>\n\
         \x20 <string>Aqua</string>\n\
         </dict>\n\
         </plist>\n"
    ))
}

pub fn plist_path(home: &Path) -> PathBuf {
    home.join("Library/LaunchAgents")
        .join(format!("{MACOS_LABEL}.plist"))
}

/// The login item's command line: the program, quoted, then its arguments.
///
/// 纯函数，跨平台测试把它钉住。程序路径一律加引号（`C:\Program Files\...` 必须
/// 有引号才不会被命令行切碎），路径里出现引号或控制字符则直接拒绝 —— 那种路径
/// 拼出来的命令行语义不确定，宁可不设自启也不写一条错的。
pub fn login_command(program: &Path, arguments: &[String]) -> Result<String> {
    if !program.is_absolute() {
        bail!("login item program must be an absolute path");
    }
    let program = program.to_string_lossy();
    if program.contains('"') || program.chars().any(char::is_control) {
        bail!("login item program path cannot be quoted safely");
    }
    let mut command = format!("\"{program}\"");
    for argument in arguments {
        // 参数是固定字面量，不该需要引号；出现空白/引号说明调用方传错了。
        if argument.is_empty()
            || argument.contains('"')
            || argument.chars().any(char::is_whitespace)
            || argument.chars().any(char::is_control)
        {
            bail!("login item arguments must be plain words");
        }
        command.push(' ');
        command.push_str(argument);
    }
    Ok(command)
}

/// 这次开关要在 Run 键里落下的终态：`Some` = 写这个命令行，`None` = 删掉这个值。
/// 拆成纯函数是为了让「写什么」和「删什么」用的是同一个键与同一个值名，不会漂移。
pub fn run_key_update(
    value: &str,
    program: Option<&Path>,
    arguments: &[String],
) -> Result<(String, Option<String>)> {
    validate_value_name(value)?;
    let command = match program {
        Some(path) => Some(login_command(path, arguments)?),
        None => None,
    };
    Ok((run_key(), command))
}

/// 值名的合法性校验（注入护栏）：只允许短字母数字串，别让调用方把任意内容写进 Run 键。
pub fn validate_value_name(value: &str) -> Result<()> {
    if value.is_empty()
        || value.len() > 64
        || !value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        bail!("invalid login item value name");
    }
    Ok(())
}

/// 当前用户的 `Run` 键。
pub fn run_key() -> String {
    "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run".to_string()
}

#[cfg(target_os = "macos")]
fn launchctl(arguments: &[&str]) -> Result<()> {
    let output = std::process::Command::new("launchctl")
        .args(arguments)
        .output()
        .context("could not run launchctl")?;
    if !output.status.success() {
        bail!("launchctl refused the login item change");
    }
    Ok(())
}

/// Enables the login item. Records the intent in settings only after the OS
/// accepted it, so the UI never claims a state the system does not have.
///
/// 返回实际生效的途径：Windows 上「写进去了」有两种可能，界面与 CLI 都要能说出来
/// （后者是企业机器上唯一能用的那条），所以这里不吞掉它。
pub fn install(root: &Path, program: &Path) -> Result<LoginItem> {
    if !program.is_absolute() {
        bail!("login item program must be an absolute path");
    }
    let arguments = vec![LOGIN_ARGUMENT.to_string()];
    #[cfg(target_os = "macos")]
    let item = {
        let home = dirs::home_dir().context("home directory unavailable")?;
        let path = plist_path(&home);
        storage::write_bytes(&path, plist(MACOS_LABEL, program, &arguments)?.as_bytes())?;
        let domain = format!("gui/{}", uid()?);
        // Bootout first: a stale job would otherwise keep the old path alive.
        let _ = launchctl(&["bootout", &domain, &path.to_string_lossy()]);
        launchctl(&["bootstrap", &domain, &path.to_string_lossy()])?;
        LoginItem::LaunchAgent
    };
    #[cfg(windows)]
    let item = install_windows(program, &arguments)?;
    #[cfg(not(any(target_os = "macos", windows)))]
    let item = other_platform_login_item()?;
    settings::update(root, |current| current.autostart = true)?;
    Ok(item)
}

/// 其余平台没有登录项后端：报错而不是静默成功（静默成功会让设置界面显示
/// 「已开启」，而系统里什么都没有）。
#[cfg(not(any(target_os = "macos", windows)))]
fn other_platform_login_item() -> Result<LoginItem> {
    bail!("login items are not implemented on this platform")
}

/// Windows：先写 `Run` 键；被拒绝（组策略锁、安全软件拦、键不可写）时退到
/// 「启动」文件夹的快捷方式。两条都失败才报错，并把两条的原因一起带出来 ——
/// 只报最后一条会让人以为「启动文件夹写不了」，而真正的原因常在 Run 键那边。
#[cfg(windows)]
fn install_windows(program: &Path, arguments: &[String]) -> Result<LoginItem> {
    let (key, command) = run_key_update(WINDOWS_VALUE, Some(program), arguments)?;
    let command = command.expect("a program was supplied");
    match crate::winreg::write_string(&key, WINDOWS_VALUE, &command) {
        Ok(()) => {
            // 首选途径已生效：把兜底途径留下的快捷方式撤掉。两条同时存在的话
            // 每次登录会起两个进程（第二个通常被单实例锁挡掉，但不该赌）。
            remove_leftover_link(program);
            Ok(LoginItem::RunKey)
        }
        Err(run_error) => match write_startup_link(program, arguments) {
            Ok(()) => Ok(LoginItem::StartupFolder),
            Err(link_error) => bail!(
                "cannot register the login item; the Run key was refused ({run_error:#}) \
                 and the Startup folder shortcut failed too ({link_error:#})"
            ),
        },
    }
}

/// 「启动」文件夹里那个快捷方式的完整路径。
#[cfg(windows)]
pub fn startup_link_path() -> Option<PathBuf> {
    crate::shortcut::startup_dir().map(|directory| directory.join(WINDOWS_LINK))
}

#[cfg(windows)]
fn write_startup_link(program: &Path, arguments: &[String]) -> Result<()> {
    let link = startup_link_path().context("the Startup folder is unavailable")?;
    // 工作目录设成程序所在目录：快捷方式从「启动」文件夹拉起时，进程的当前目录
    // 会是那里，而不是安装目录 —— 得显式纠回来。
    let working = program
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
        .context("login item program has no directory")?;
    crate::shortcut::write(&link, program, arguments, working, WINDOWS_DESCRIPTION)
}

/// 清掉兜底途径留下的快捷方式 —— **只在它确实指向本程序时**才删。
/// 读不出目标（不是快捷方式 / shell 拒绝）就一律不碰：用户自己放在「启动」里的
/// 同名快捷方式不能因为名字撞上就被删掉。
#[cfg(windows)]
fn remove_leftover_link(program: &Path) {
    let Some(link) = startup_link_path() else {
        return;
    };
    if !link.exists() {
        return;
    }
    if crate::shortcut::target(&link).is_some_and(|target| same_path(&target, program)) {
        let _ = crate::shortcut::remove(&link);
    }
}

/// 同一个文件的两种写法。比较的是**路径**，所以统一分隔符并忽略大小写
/// （Windows 文件系统不区分大小写，shell 回读时可能返回 `\` 也可能是 `/`）。
pub fn same_path(left: &Path, right: &Path) -> bool {
    let normalize = |path: &Path| path.to_string_lossy().replace('/', "\\").to_lowercase();
    normalize(left) == normalize(right)
}

pub fn remove(root: &Path) -> Result<()> {
    #[cfg(target_os = "macos")]
    {
        let home = dirs::home_dir().context("home directory unavailable")?;
        let path = plist_path(&home);
        if path.exists() {
            let domain = format!("gui/{}", uid()?);
            let _ = launchctl(&["bootout", &domain, &path.to_string_lossy()]);
            std::fs::remove_file(&path)?;
        }
    }
    #[cfg(windows)]
    {
        // 只删自己写的那一个值，绝不动 Run 键里别人的条目；值本来就不在也算成功。
        let (key, _) = run_key_update(WINDOWS_VALUE, None, &[])?;
        crate::winreg::delete_value(&key, WINDOWS_VALUE)?;
        // 两条途径都要撤：装的时候可能走的是兜底那条，也可能两条都留下过。
        // 快捷方式名只属于本安装（`CloudFileAgent22.lnk`），与 Go 侧卸载一致。
        if let Some(link) = startup_link_path() {
            crate::shortcut::remove(&link)?;
        }
    }
    settings::update(root, |current| current.autostart = false)?;
    Ok(())
}

/// Reads the operating system, not the settings mirror: the settings file is
/// only ever a record of what the agent asked for.
pub fn enabled() -> bool {
    mechanism().is_some()
}

/// 登录项目前在哪一条途径上；没设则 `None`。与 [`enabled`] 同源（`enabled` 就是
/// 「有没有途径」），所以两者不可能互相矛盾。
pub fn mechanism() -> Option<LoginItem> {
    #[cfg(target_os = "macos")]
    {
        let home = dirs::home_dir()?;
        plist_path(&home)
            .is_file()
            .then_some(LoginItem::LaunchAgent)
    }
    #[cfg(windows)]
    {
        if crate::winreg::read_string(&run_key(), WINDOWS_VALUE).is_some() {
            return Some(LoginItem::RunKey);
        }
        let link = startup_link_path()?;
        link.is_file().then_some(LoginItem::StartupFolder)
    }
    #[cfg(not(any(target_os = "macos", windows)))]
    {
        None
    }
}

#[cfg(target_os = "macos")]
fn uid() -> Result<u32> {
    let output = std::process::Command::new("id").arg("-u").output()?;
    let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
    value.parse().context("could not determine the user id")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Absolute paths for the host OS. `Path::is_absolute` is false for `/opt/x`
    /// on Windows, so a Unix literal passes on macOS and is refused by the
    /// login-item builders on the Windows runner.
    #[cfg(windows)]
    const PROGRAM: &str = "C:/Program Files/CloudFile Agent/cloudfile-desktop.exe";
    #[cfg(not(windows))]
    const PROGRAM: &str = "/opt/CloudFile Agent/bin/cloudfile-desktop";

    /// Same, for the escaping test, which needs an `&` inside the path.
    #[cfg(windows)]
    const AMPERSAND: &str = "C:/a&b/bin";
    #[cfg(not(windows))]
    const AMPERSAND: &str = "/opt/a&b/bin";

    /// Same, for the registry argument test, which needs an `.exe` suffix.
    #[cfg(windows)]
    const EXE: &str = "C:/CloudFileAgent22/cloudfile-desktop.exe";
    #[cfg(not(windows))]
    const EXE: &str = "/opt/CloudFileAgent22/cloudfile-desktop.exe";

    #[test]
    fn plist_is_a_well_formed_login_item() {
        let rendered = plist(
            MACOS_LABEL,
            Path::new(PROGRAM),
            &["--autostart".to_string()],
        )
        .unwrap();
        assert!(rendered.starts_with("<?xml version=\"1.0\""));
        assert!(rendered.contains("<string>com.cloudfile.agent22.autostart</string>"));
        assert!(rendered.contains(&format!("<string>{PROGRAM}</string>")));
        assert!(rendered.contains("<string>--autostart</string>"));
        assert!(rendered.contains("<key>RunAtLoad</key>"));
        assert!(rendered.contains("<key>SuccessfulExit</key>"));
        assert!(rendered.contains("<string>Aqua</string>"));
        assert!(rendered.contains("</plist>"));
        assert!(plist(DEV_LABEL_WITH_ESCAPE, Path::new("/a&b/c"), &[]).is_err());
        assert!(plist(MACOS_LABEL, Path::new("relative"), &[]).is_err());
    }

    const DEV_LABEL_WITH_ESCAPE: &str = "bad label";

    #[test]
    fn escaping_prevents_a_broken_property_list() {
        let plist = plist(MACOS_LABEL, Path::new(AMPERSAND), &["--x".into()]).unwrap();
        assert!(plist.contains(&AMPERSAND.replace('&', "&amp;")));
        assert!(!plist.contains(AMPERSAND));
    }

    #[test]
    fn login_item_path_is_per_user() {
        assert_eq!(
            plist_path(Path::new("/Users/user")),
            Path::new("/Users/user/Library/LaunchAgents/com.cloudfile.agent22.autostart.plist")
        );
    }

    #[test]
    fn run_key_update_is_exact_and_reversible() {
        let arguments = vec![LOGIN_ARGUMENT.to_string()];
        // 写：键 + 带引号的命令行（路径必须带引号，参数跟在后面）。
        let (key, command) =
            run_key_update(WINDOWS_VALUE, Some(Path::new(EXE)), &arguments).unwrap();
        assert_eq!(
            key,
            "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run"
        );
        assert_eq!(
            command.as_deref(),
            Some(format!("\"{EXE}\" --autostart").as_str())
        );
        // 删：同一个键，值为 None —— 只删自己那个值，不删键。
        let (key, command) = run_key_update(WINDOWS_VALUE, None, &[]).unwrap();
        assert_eq!(
            key,
            "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run"
        );
        assert_eq!(command, None);
        // 注入护栏与路径校验。
        assert!(run_key_update("bad name", None, &[]).is_err());
        assert!(run_key_update(WINDOWS_VALUE, Some(Path::new("relative.exe")), &[]).is_err());
        assert!(validate_value_name("CloudFileAgent22").is_ok());
        assert!(validate_value_name("").is_err());
        assert!(validate_value_name(&"x".repeat(65)).is_err());
    }

    #[test]
    fn the_login_command_quotes_the_program_and_carries_the_argument() {
        let arguments = vec![LOGIN_ARGUMENT.to_string()];
        assert_eq!(
            login_command(Path::new(PROGRAM), &arguments).unwrap(),
            format!("\"{PROGRAM}\" --autostart")
        );
        // 没有参数时只有被引起来的程序路径（不留尾随空格）。
        assert_eq!(
            login_command(Path::new(PROGRAM), &[]).unwrap(),
            format!("\"{PROGRAM}\"")
        );
        // 相对路径、内嵌引号、可疑参数一律拒绝：宁可不起自启，也不写一条
        // 语义不确定的命令行。
        assert!(login_command(Path::new("relative.exe"), &arguments).is_err());
        assert!(login_command(Path::new(EXE), &["two words".into()]).is_err());
        assert!(login_command(Path::new(EXE), &["\"quoted\"".into()]).is_err());
        assert!(login_command(Path::new(EXE), &[String::new()]).is_err());
    }

    #[test]
    fn login_item_ids_are_stable_and_specific() {
        // 这些字符串会出现在 `--status` 的 JSON 和设置界面里，改名等于改契约。
        assert_eq!(LoginItem::RunKey.id(), "run-key");
        assert_eq!(LoginItem::StartupFolder.id(), "startup-folder");
        assert_eq!(LoginItem::LaunchAgent.id(), "launch-agent");
    }

    /// 「启动」文件夹里的快捷方式名与服务端/界面看到的名字要对得上，
    /// 且必须能一眼看出是本安装的（删除时只按名字认领）。
    #[cfg(windows)]
    #[test]
    fn the_startup_shortcut_is_named_after_this_install() {
        assert_eq!(WINDOWS_LINK, format!("{WINDOWS_VALUE}.lnk"));
        let link = startup_link_path().expect("the Startup folder should be known");
        assert!(link.ends_with(WINDOWS_LINK));
        assert!(link.is_absolute());
        assert!(link
            .parent()
            .is_some_and(|parent| parent.ends_with("Startup")));
    }

    #[test]
    fn the_same_file_written_two_ways_compares_equal() {
        // shell 回读时可能给 `\` 或 `/`，也可能改变大小写。
        assert!(same_path(
            Path::new("C:/Program Files/CloudFile Agent/cloudfile-desktop.exe"),
            Path::new("c:\\program files\\cloudfile agent\\cloudfile-desktop.exe")
        ));
        assert!(!same_path(
            Path::new("C:/Program Files/CloudFile Agent/cloudfile-desktop.exe"),
            Path::new("C:/Program Files/Other/cloudfile-desktop.exe")
        ));
    }
}
