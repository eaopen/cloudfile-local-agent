//! CloudFile Agent 2.2 的引导安装体（bootstrap）。
//!
//! 空白机器上没有任何可运行的东西，而 `cloudfile-native-host --install` 需要一个**已存在的**
//! 二进制来跑。这个程序就是那个「先有鸡」的小东西，对应 Go 侧的 `install.cmd`：
//!
//! 1. 定源：主源不通就用备用源（直连静态服务的 IP），与 `--install` 同一套探测逻辑。
//! 2. 取**已签名**的发布清单，用内置的钉住公钥验签。
//! 3. 下载 `cloudfile-native-host`，校验清单里的 sha256。
//! 4. 把参数原样转给它：`cloudfile-native-host --install --source <可用源> ...`。
//!
//! 它自己**不带任何负载**、不做注册也不做替换：注册、暂存、健康检查、激活、自启、启动
//! 全都由 native host 完成（安全顺序只有一份实现）。这样这个程序小到可以随邮件发出去，
//! 而所有会改机器状态的动作仍然集中在一个被测试覆盖的地方。
//!
//! 它与 Go 的 `install.cmd` 有一点本质区别：`install.cmd` 从通道下载 PowerShell 脚本并
//! **无签名**地执行它（只查 sha256）。这里必须先验签才下载任何字节 —— 信任锚是内置公钥，
//! 不是通道自己。公钥在**打包时注入**（`CLOUDFILE_INSTALL_PUBLIC_KEY`），
//! 绝不在运行时从通道取：否则「谁签发」就变成了「通道说了算」。

use anyhow::{bail, Context, Result};
use cloudfile_core::update;
use serde_json::json;
use std::path::{Path, PathBuf};

/// 通道名（发版时改这一行，与 Go 的 install.cmd 里 `CHANNEL` 同一个作用）。
const CHANNEL: &str = "cloudfile-updates";
/// 主源：域名（可换域名/证书，机器上要能解析）。
const HOST_PRIMARY: &str = "etech.stcetech.ad01.sec.com";
/// 备用源：直连静态服务的 IP（DNS 或代理出问题时用）。
const HOST_MIRROR: &str = "10.9.8.162:6111";

/// 打包时注入的钉住公钥（base64url 的未压缩 P-256 点）。
/// **空着就只能在命令行显式给 `--public-key`**，绝不悄悄从通道取。
const PINNED_KEY: Option<&str> = option_env!("CLOUDFILE_INSTALL_PUBLIC_KEY");

const USAGE: &str = "\
CloudFile Agent 2.2 bootstrap

Usage: cloudfile-install [options] [-- <extra cloudfile-native-host --install options>]

  --source <url>            primary update origin (defaults to the built-in channel)
  --source-fallback <url>   mirror used when the primary is unreachable
  --public-key <base64url>  pinned P-256 release key (defaults to the built-in one)
  --install-dir <path>      install root (defaults to %LOCALAPPDATA%\\CloudFileAgent22,
                            or $CLOUDFILE_AGENT2_HOME)
  --dry-run                 resolve the source and print the plan; changes nothing
  --no-pause                do not wait for Enter at the end (double-click behaviour)
  --help, -h                print this message

Everything after the options is forwarded to `cloudfile-native-host --install`
(for example --extension-id <id>, --skip-autostart, --takeover).
";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|value| value == "--help" || value == "-h") {
        print!("{USAGE}");
        return;
    }
    // 双击启动时（没有参数、终端上跑）要把窗口留住：否则一闪而过，用户什么也看不到。
    // 自动化里没有终端，所以不会挂住。
    let pause = should_pause(&args);
    let code = match run(&args) {
        Ok(code) => code,
        Err(error) => {
            eprintln!("\n安装失败：{error:#}");
            1
        }
    };
    if pause {
        eprintln!("\n按回车键退出。");
        let mut line = String::new();
        let _ = std::io::stdin().read_line(&mut line);
    }
    std::process::exit(code);
}

/// 只在「双击启动」这一种情况下等回车：没有参数、没有 `--no-pause`、标准输入是终端。
fn should_pause(args: &[String]) -> bool {
    use std::io::IsTerminal;
    args.is_empty()
        && std::env::var_os("CLOUDFILE_NO_PAUSE").is_none()
        && std::io::stdin().is_terminal()
}

fn run(args: &[String]) -> Result<i32> {
    let (options, forwarded) = split_options(args)?;
    let dry_run = options.dry_run;

    // 1) 装在哪儿，以及「装得进去吗」。与 --install 用同一个判据（建目录 + 写探针）。
    let install = install_dir(&options)?;
    if !dry_run {
        ensure_writable(&install)?;
    }

    // 2) 定源：主源 → 备用源，把实际可用的那个交给 native host 写回配置。
    let policy = if options.allow_insecure {
        update::SourcePolicy::DEV
    } else {
        update::SourcePolicy::STRICT
    };
    let (source, probes) = update::pick_source(policy, &options.candidates());
    let Some(source) = source else {
        bail!(
            "两个更新源都不可用：{}\n请检查本机能否访问上面的地址（DNS/代理/防火墙）后重试。",
            update::describe_probes(&probes)
        );
    };
    eprintln!("通道：{source}");

    if dry_run {
        // 只报告，不改机器：现场诊断「这台机器能不能连上通道」用。
        // 也报出**内置的那把公钥** —— 发布方与现场都要能一眼确认「这个引导体钉的是哪把钥匙」，
        // 否则只有等真的安装到一半报「公钥不符」才发现内置错了。公钥是公开信息，不是秘密。
        let pinned = PINNED_KEY.filter(|key| !key.trim().is_empty());
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "dry_run": true,
                "install_dir": install.to_string_lossy(),
                "update_source": source,
                "allow_insecure_update_source": policy.allow_insecure,
                "source_probes": probes.iter().map(|probe| json!({
                    "source": probe.source,
                    "error": probe.error,
                })).collect::<Vec<_>>(),
                "pinned_public_key_set": pinned.is_some(),
                "pinned_public_key": pinned.unwrap_or(""),
                "forwarded_to_native_host": forwarded,
            }))?
        );
        return Ok(0);
    }

    // 3) 公钥：内置的优先，命令行可以覆盖。**没有就停在这里**，连清单都不去取
    //    （`--dry-run` 已经在上一步返回了：那种诊断不该被「没配公钥」挡住）。
    let public_key = options
        .public_key
        .clone()
        .or_else(|| PINNED_KEY.map(str::to_string))
        .filter(|key| !key.trim().is_empty())
        .context(
            "没有可用的发布公钥：请用 --public-key <base64url>，或在打包时注入 \
             CLOUDFILE_INSTALL_PUBLIC_KEY（引导体不接受「从通道拿公钥」，那等于让通道自己给自己签发）",
        )?;
    // 提前验证公钥本身合法：等下载完再报「公钥不对」就太晚了。
    update::verifying_key(&public_key).context("内置/传入的发布公钥不可用")?;

    // 4) 取清单、验签、下载 native host。顺序与 native host 自己升级时完全一致：
    //    签名通过之前不下载任何字节，摘要对不上就不写盘。
    let release = update::parse_release(&update::fetch_manifest_for(policy, &source)?)
        .context("更新清单无法解析")?;
    update::verify(
        policy,
        &release,
        &source,
        // 引导安装时这台机器上还没有任何版本可比，所以「必须比当前新」这条不适用：
        // 用一个比任何合法版本都小的基准，其余校验（平台、协议、有效期、签名、同源）一条不少。
        NO_INSTALLED_VERSION,
        cloudfile_core::now(),
        &update::platform_id(std::env::consts::OS, std::env::consts::ARCH),
        &public_key,
    )
    .context("更新清单没有通过验签")?;
    let artifact = release
        .artifacts
        .iter()
        .find(|artifact| artifact.kind == "native-host")
        .context("清单里没有 native host 组件")?;
    let bytes = update::download(artifact).context("下载 native host 失败")?;

    // 副本直接放在安装根里：native host 用「可执行文件的位置」推导安装根
    // （`install_root` 会向上找 `bin` 那一层，找不到就用 exe 的父目录），
    // 放在子目录里会让版本目录长到那个子目录下面去。装成功后这个副本会被删掉。
    let binary = install.join(host_file_name());
    cloudfile_core::storage::write_bytes(&binary, &bytes)?;
    eprintln!("已下载 {}（{} 字节）", release.version, bytes.len());

    // 5) 交给它。注册、暂存、健康检查、激活、自启、启动都在那边做。
    let code = hand_off(&binary, &install, &source, policy, &public_key, &forwarded)?;
    if code == 0 {
        // 装成功就把引导副本删掉：真正的安装已经在版本目录里，注册指向的是那一份。
        let _ = std::fs::remove_file(&binary);
    } else {
        // 失败时留着：现场可以直接重跑这个副本、看着它的输出排查。
        eprintln!(
            "native host 安装没有成功（退出码 {code}）；引导副本保留在 {}",
            binary.display()
        );
    }
    Ok(code)
}

/// 这台机器上「还没有版本」的基准。比任何合法版本都小，所以「清单必须更新」永远成立，
/// 而其余所有校验照旧 —— 引导安装不该因为「装的正好是同一个版本」而被拒绝。
const NO_INSTALLED_VERSION: &str = "0.0.0";

struct Options {
    source: Option<String>,
    fallback: Option<String>,
    public_key: Option<String>,
    install_dir: Option<PathBuf>,
    dry_run: bool,
    allow_insecure: bool,
}

impl Options {
    /// 候选源：显式给的优先，否则用内置的通道地址（主源 + 备用源）。
    fn candidates(&self) -> Vec<String> {
        match (&self.source, &self.fallback) {
            (Some(source), fallback) => {
                let mut list = vec![source.clone()];
                list.extend(fallback.clone());
                list
            }
            (None, _) => vec![
                format!("https://{HOST_PRIMARY}/{CHANNEL}"),
                format!("https://{HOST_MIRROR}/{CHANNEL}"),
            ],
        }
    }
}

/// 分开「引导体自己的选项」与「要转给 native host --install 的选项」。
/// 认不出来的参数一律转发（`--extension-id`、`--skip-*`、`--takeover` 都归那边管），
/// 只是要把它们的取值一起带过去，否则那边会收到一个光秃秃的开关。
fn split_options(args: &[String]) -> Result<(Options, Vec<String>)> {
    let mut options = Options {
        source: None,
        fallback: None,
        public_key: None,
        install_dir: None,
        dry_run: false,
        allow_insecure: false,
    };
    let mut forwarded = Vec::new();
    let mut index = 0;
    while index < args.len() {
        let argument = args[index].as_str();
        let take_value = |index: &mut usize| -> Result<String> {
            *index += 1;
            args.get(*index)
                .cloned()
                .with_context(|| format!("{argument} 缺少取值"))
        };
        match argument {
            "--source" => options.source = Some(take_value(&mut index)?),
            "--source-fallback" => options.fallback = Some(take_value(&mut index)?),
            "--public-key" => options.public_key = Some(take_value(&mut index)?),
            "--install-dir" => options.install_dir = Some(PathBuf::from(take_value(&mut index)?)),
            "--dry-run" => options.dry_run = true,
            "--no-pause" => {}
            "--allow-insecure-source" => {
                options.allow_insecure = true;
                forwarded.push(argument.to_string());
            }
            "--" => {
                forwarded.extend(args[index + 1..].iter().cloned());
                break;
            }
            other if other.starts_with('-') => {
                // 转发的开关**连同它的取值**一起带走：取值以 `-` 开头的概率为零，
                // 但 `--extension-id <id>` 这样的成对参数必须整对过去。
                forwarded.push(other.to_string());
                if let Some(value) = args.get(index + 1).filter(|value| !value.starts_with('-')) {
                    forwarded.push(value.clone());
                    index += 1;
                }
            }
            other => bail!("无法识别的参数 {other}\n\n{USAGE}"),
        }
        index += 1;
    }
    Ok((options, forwarded))
}

/// 安装根：命令行 > `CLOUDFILE_AGENT2_HOME` > 平台默认（与 Agent 自己算的一致）。
fn install_dir(options: &Options) -> Result<PathBuf> {
    if let Some(directory) = &options.install_dir {
        if !directory.is_absolute() {
            bail!("--install-dir 必须是绝对路径");
        }
        return Ok(directory.clone());
    }
    if let Some(home) = std::env::var_os("CLOUDFILE_AGENT2_HOME") {
        if !home.is_empty() {
            return Ok(PathBuf::from(home));
        }
    }
    cloudfile_core::storage::root().context("无法确定安装目录")
}

fn ensure_writable(install: &Path) -> Result<()> {
    std::fs::create_dir_all(install)
        .with_context(|| format!("创建安装目录失败：{}", install.display()))?;
    let probe = install.join(".cf-write-test");
    std::fs::write(&probe, b"ok")
        .with_context(|| format!("安装目录不可写：{}", install.display()))?;
    let _ = std::fs::remove_file(&probe);
    Ok(())
}

/// 平台上的 host 文件名。清单里的 artifact 是**不带版本后缀**的稳定名字，
/// 版本体现在目录上（`<install>/bin/<version>/`），所以这里不需要版本号。
fn host_file_name() -> &'static str {
    if cfg!(windows) {
        "cloudfile-native-host.exe"
    } else {
        "cloudfile-native-host"
    }
}

/// 运行刚下回来的 native host，把 `--install` 与转发的参数交给它，
/// 并（Windows 上）顺手把 Chrome 需要的清单名一起告诉它 —— 那部分由它自己决定。
fn hand_off(
    binary: &Path,
    install: &Path,
    source: &str,
    policy: update::SourcePolicy,
    public_key: &str,
    forwarded: &[String],
) -> Result<i32> {
    let mut command = std::process::Command::new(binary);
    command
        .arg("--install")
        .arg("--source")
        .arg(source)
        .env("CLOUDFILE_AGENT2_HOME", install)
        // 让 native host 用同一套源策略：引导体已经按这台机器的情况判定过，
        // 这里只是把结论传下去（它仍然会自己再校验一遍）。
        .args(if policy.allow_insecure {
            vec!["--allow-insecure-source"]
        } else {
            Vec::new()
        });
    // 把**生效的**公钥传下去（内置的或命令行给的都算）：native host 要用它写进配置，
    // 否则正式通道会因为「有源没公钥」被它自己拒绝。
    command.args(["--public-key", public_key]);
    command.args(forwarded);
    eprintln!("运行：{} --install --source {source}", binary.display());
    let status = command.status().context("无法启动 native host")?;
    Ok(status.code().unwrap_or(1))
}
