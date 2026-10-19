pub mod apps;
pub mod autostart;
pub mod guard;
pub mod hostreg;
pub mod ipc;
pub mod journal;
pub mod native;
pub mod session;
pub mod settings;
pub mod storage;
pub mod update;
pub mod wire;

/// 子进程控制台窗口控制（其它平台是空操作）。
pub mod childproc;
/// 「启动」文件夹里的快捷方式（`.lnk`），仅 Windows —— 登录项的兜底途径。
#[cfg(windows)]
pub mod shortcut;
/// 进程内注册表访问，仅 Windows —— 用来替代 spawn `reg.exe`。
#[cfg(windows)]
pub mod winreg;
/// 本地工作副本（镜像）根目录的策略与一次性搬迁。
pub mod workspace;

/// 本构建自称的版本号。
///
/// 默认取工作区的 `Cargo.toml`（开发时手跑 `cargo build` 得到的就是它）；发版时由
/// `CLOUDFILE_AGENT2_VERSION` **在编译期**注入 —— 等价于 Go 侧的
/// `-ldflags "-X main.version=..."`。这样做而不是让发版脚本改 `Cargo.toml`，是因为
/// 升级链路上有三处会把这个字符串当成事实来比较：清单里的 `version`、
/// `update::probe` 拿到候选二进制 `--version` 后的比对、以及 `stage_release` 里
/// 「报告版本必须等于清单版本」。三者必须同源，而改仓库文件的方式一旦中途失败
/// 就会留下一个「构建出来的版本 ≠ 签发的版本」的中间态。
///
/// 值必须能通过 `update::valid_version`（`x.y.z`，可带 `-预发布`）；发版脚本会在
/// 构建**之前**校验这一点，因为带着非法版本号的构建连自己的健康检查都过不去。
pub const VERSION: &str = match option_env!("CLOUDFILE_AGENT2_VERSION") {
    Some(version) => version,
    None => env!("CARGO_PKG_VERSION"),
};
/// 本平台注册的 Native Messaging Host ID。定义在 `hostreg`，这里只做转出 ——
/// 「注册用哪个名字」和「自带检查查哪个名字」必须是同一个常量，否则自检会在
/// 一个根本没被注册的 host 上给出「未注册」，而扩展其实连得上。
pub const HOST: &str = hostreg::HOST;
pub fn now() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

pub mod auto_upload;
pub mod cdc;
pub mod device;
pub mod exclusive;
/// 浏览器扩展的自更新（下载 + 校验 + 原子替换目录）。
pub mod extension;
/// 「只有属主能碰吗」—— 读侧加固的唯一判据（对应 Go 的 `internal/fsaccess`）。
pub mod fsaccess;
pub mod paired_session;
/// 本机自检：把「这台机器上装的东西到底能不能工作」变成一次可脚本化的问答。
pub mod selftest;
pub mod upload;
