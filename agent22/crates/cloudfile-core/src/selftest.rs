//! 本机自检：把「这台机器上装的东西到底能不能工作」变成一次可脚本化的问答。
//!
//! 存在的理由和 Go 侧任务中心的 `--self-test` 一样：客户机上出问题时，
//! 「哪个环节坏了」要从外面看得见。因此每一项都**独立**跑、独立报，一项失败不影响
//! 其余项，输出是稳定的 JSON（`{"ok":…,"checks":[{"name","ok","detail"}]}`）。
//!
//! 刻意**不创建窗口、不发网络请求**：
//!
//! - 创建窗口会依赖窗口站/桌面会话，在服务、SSH、CI 上必然不稳；自检本身变成
//!   「时好时坏」就没有价值了。图形栈的问题由人眼和使用现场去发现。
//! - 网络请求要等超时，会把「10 秒出结论」变成「30 秒还不一定出结论」。更新源只报
//!   「配没配」，配了的内容是否正确由 `--check-update` 回答。
//!
//! 失败项一律带原因（`detail`），而且原因来自真实的系统调用错误，不是猜测。

use crate::{apps, extension, guard, hostreg, ipc, session, storage, update};
use serde_json::{json, Value};
use std::path::Path;

/// 一项检查的结果。`ok=false` 表示这一环现在**确实不工作**，而不是「未配置」。
struct Check {
    name: &'static str,
    ok: bool,
    detail: String,
}

impl Check {
    fn pass(name: &'static str, detail: impl Into<String>) -> Self {
        Check {
            name,
            ok: true,
            detail: detail.into(),
        }
    }
    fn fail(name: &'static str, detail: impl Into<String>) -> Self {
        Check {
            name,
            ok: false,
            detail: detail.into(),
        }
    }
    /// 「能读到、但状态由人判断」的项：永远不算失败，只把事实报出来。
    fn report(name: &'static str, detail: impl Into<String>) -> Self {
        Check::pass(name, detail)
    }
}

/// 跑完所有检查。`native_host` 是调用方认为的 native host 路径（桌面进程与它同级）。
pub fn run(root: &Path, native_host: &Path) -> Value {
    let checks = vec![
        private_store(root),
        mailbox(root),
        single_instance(root),
        peer_binary(native_host),
        component(),
        registration(),
        task_store(root),
        exit_assessment(root),
        webview(),
        browser_extension(root),
        login_item(),
        update_state(root),
    ];
    let ok = checks.iter().all(|check| check.ok);
    json!({
        "ok": ok,
        "root": root.to_string_lossy(),
        "checks": checks.iter().map(|check| json!({
            "name": check.name,
            "ok": check.ok,
            "detail": check.detail,
        })).collect::<Vec<Value>>(),
    })
}

/// 私有目录能不能真写进去。写的是自己目录下一个固定名字的探针文件，写完即删；
/// 留下它也不影响任何逻辑（下次会被覆盖）。
fn private_store(root: &Path) -> Check {
    let probe = root.join("self-test.json");
    let body = json!({"at": crate::now()});
    match storage::write(&probe, &body).and_then(|()| {
        let read: Result<Value, _> = storage::read(&probe);
        std::fs::remove_file(&probe).ok();
        read
    }) {
        Ok(_) => Check::pass("private-store", format!("可读写 {}", root.display())),
        Err(error) => Check::fail("private-store", format!("{error:#}")),
    }
}

/// 邮箱目录能不能建、中断的请求能不能收尾。收尾失败会让扩展一直等一个永不出现的应答。
fn mailbox(root: &Path) -> Check {
    match ipc::recover(root) {
        Ok(()) => Check::pass("mailbox", "中断的请求已收尾"),
        Err(error) => Check::fail("mailbox", format!("{error:#}")),
    }
}

/// 单实例锁。**已经有一个活着的实例在跑不算失败**——那正是它该有的样子；
/// 只有拿不到锁又说不出「谁在占用」才是异常。
fn single_instance(root: &Path) -> Check {
    let path = root.join("self-test.lock");
    match storage::lock(&path) {
        Ok(file) => {
            drop(file);
            let _ = std::fs::remove_file(&path);
            Check::pass("single-instance", "可以取得实例锁")
        }
        Err(error) => Check::report(
            "single-instance",
            format!("锁已被占用（通常意味着已有实例在运行）：{error:#}"),
        ),
    }
}

/// 同级的搭档程序：桌面进程缺了 native host 就是半个安装（Chrome 注册指向后者），
/// native host 缺了桌面则用户没有任何入口。两个入口各自声明自己认哪个搭档是必需的。
fn peer_binary(peer: &Path) -> Check {
    if peer.is_file() {
        Check::pass("peer-binary", peer.to_string_lossy())
    } else {
        Check::fail("peer-binary", format!("找不到搭档程序 {}", peer.display()))
    }
}

/// 本程序自己声明的协议版本。升级前的健康检查读的就是这两个数，
/// 说不出来等于不可用。
fn component() -> Check {
    if update::IPC_VERSION == 0 || update::JOURNAL_VERSION == 0 {
        return Check::fail("component", "协议版本未声明");
    }
    Check::pass(
        "component",
        format!(
            "{} / ipc {} / journal {}",
            crate::VERSION,
            update::IPC_VERSION,
            update::JOURNAL_VERSION
        ),
    )
}

/// Chrome host 注册状态。没注册不算失败（用户可能正在用命令行），但必须读得出来。
fn registration() -> Check {
    let dir = match hostreg::manifest_dir() {
        Ok(dir) => dir,
        Err(error) => return Check::fail("registration", format!("{error:#}")),
    };
    match hostreg::state(&dir, crate::HOST, None) {
        Ok(state) => {
            let detail = if state.foreign {
                format!("{} 由其他程序注册（可显式接管）", state.host)
            } else if let Some(binary) = state.binary {
                format!("已注册 -> {}", binary.display())
            } else {
                format!("{} 未注册", state.host)
            };
            Check::report("registration", detail)
        }
        Err(error) => Check::fail("registration", format!("{error:#}")),
    }
}

/// 任务目录能不能读。这里同时暴露「工作副本根被搬到了哪」——排查现场时第一个要问的。
fn task_store(root: &Path) -> Check {
    let dir = crate::workspace::tasks_root(root);
    match session::tasks(root) {
        Ok(tasks) => Check::pass(
            "task-store",
            format!("{} 个任务，目录 {}", tasks.len(), dir.display()),
        ),
        Err(error) => Check::fail("task-store", format!("{error:#}")),
    }
}

/// 退出判定必须总能给出答案（这是升级与退出的唯一依据）。
fn exit_assessment(root: &Path) -> Check {
    let assessment = guard::assess(root);
    Check::pass(
        "exit-assessment",
        format!(
            "safe_to_exit={} 进行中={} 不确定={}",
            assessment.safe_to_exit, assessment.active_checkouts, assessment.uncertain
        ),
    )
}

/// Windows 的桌面外壳**依赖** WebView2，缺了就是打不开窗口。macOS 用系统 WebKit，
/// 永远在，所以那一侧只需说明「不适用」。
fn webview() -> Check {
    match apps::webview2_version() {
        Some(version) => Check::pass("webview2", version),
        None if cfg!(windows) => Check::fail(
            "webview2",
            "未检测到 WebView2 运行时；桌面任务中心无法打开窗口",
        ),
        None => Check::report("webview2", "不适用（使用系统 WebKit）"),
    }
}

/// 浏览器扩展副本：装了哪个版本、目录在哪。没配目录不是失败（扩展可能由商店更新）。
fn browser_extension(root: &Path) -> Check {
    let config = session::config(root).unwrap_or_default();
    let Some(directory) = config.extension_dir else {
        return Check::report("extension", "未配置本机扩展目录（不影响扩展商店版本）");
    };
    let dir = Path::new(&directory);
    match extension::installed_version(dir) {
        Some(version) => Check::pass("extension", format!("{version}（{}）", dir.display())),
        None => Check::fail(
            "extension",
            format!("{} 里读不到 manifest.json 的版本", dir.display()),
        ),
    }
}

/// 登录项在哪条途径上。没设不是失败：自启一直是可选的。
fn login_item() -> Check {
    match crate::autostart::mechanism() {
        Some(item) => Check::report("login-item", item.id()),
        None => Check::report("login-item", "未设置开机自启"),
    }
}

/// 更新源配没配。**只报事实**：Rust 侧要求规范 https origin，
/// 配了明文 http 的话这里会明说「不可用」，免得等真的去升级才发现。
fn update_state(root: &Path) -> Check {
    let config = session::config(root).unwrap_or_default();
    let Some(source) = config.update_source else {
        return Check::report("update-source", "未配置");
    };
    if !update::valid_source(&source) {
        return Check::fail(
            "update-source",
            format!("{source} 不是规范 https origin（形如 https://host/dir），更新不可用"),
        );
    }
    let signed = config.update_public_key.is_some();
    Check::report(
        "update-source",
        format!(
            "{source}（{}）",
            if signed {
                "Agent 与扩展更新均可用"
            } else {
                "只有扩展更新可用：缺少签名公钥，Agent 自身不可升级"
            }
        ),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn checks(value: &Value) -> Vec<(String, bool, String)> {
        value["checks"]
            .as_array()
            .unwrap()
            .iter()
            .map(|check| {
                (
                    check["name"].as_str().unwrap().to_string(),
                    check["ok"].as_bool().unwrap(),
                    check["detail"].as_str().unwrap().to_string(),
                )
            })
            .collect()
    }

    fn find<'a>(value: &'a Value, name: &str) -> &'a Value {
        value["checks"]
            .as_array()
            .unwrap()
            .iter()
            .find(|check| check["name"] == name)
            .unwrap_or_else(|| panic!("{name} 不在报告里"))
    }

    #[test]
    fn every_check_reports_and_the_report_shape_is_stable() {
        let (_dir, root) = storage::test_root();
        let host = root.join("cloudfile-native-host");
        std::fs::write(&host, b"stub").unwrap();
        let report = run(&root, &host);

        // 名字固定：现场排查时脚本按名字取值，改名等于改接口。
        let names: Vec<String> = checks(&report).into_iter().map(|c| c.0).collect();
        assert_eq!(
            names,
            vec![
                "private-store",
                "mailbox",
                "single-instance",
                "peer-binary",
                "component",
                "registration",
                "task-store",
                "exit-assessment",
                "webview2",
                "extension",
                "login-item",
                "update-source",
            ]
        );
        assert!(report["ok"].is_boolean());
        assert_eq!(report["root"], json!(root.to_string_lossy()));
        // 探针文件不许留下。
        assert!(!root.join("self-test.json").exists());
        assert!(!root.join("self-test.lock").exists());
    }

    #[test]
    fn a_missing_peer_binary_is_a_failure_with_the_path_in_the_reason() {
        let (_dir, root) = storage::test_root();
        let host = root.join("cloudfile-native-host");
        assert!(!find(&run(&root, &host), "peer-binary")["ok"]
            .as_bool()
            .unwrap());
        assert!(find(&run(&root, &host), "peer-binary")["detail"]
            .as_str()
            .unwrap()
            .contains("cloudfile-native-host"));

        std::fs::write(&host, b"stub").unwrap();
        let report = run(&root, &host);
        assert!(find(&report, "peer-binary")["ok"].as_bool().unwrap());
    }

    #[test]
    fn a_plain_http_update_source_is_reported_as_unusable() {
        let (_dir, root) = storage::test_root();
        let host = root.join("host");
        std::fs::write(&host, b"stub").unwrap();
        let config = session::Config {
            allowed_origins: Vec::new(),
            service_paths: Default::default(),
            // 与开发通道现状一致：明文 http + 指向清单文件而不是 origin。
            update_source: Some("http://10.9.8.162:6111/cloudfile-updates-dev/update.json".into()),
            update_public_key: None,
            extension_dir: None,
        };
        storage::write(&root.join("config.json"), &config).unwrap();

        let report = run(&root, &host);
        let check = find(&report, "update-source");
        assert!(!check["ok"].as_bool().unwrap(), "{check}");
        assert!(
            check["detail"].as_str().unwrap().contains("https"),
            "{check}"
        );
        assert!(!report["ok"].as_bool().unwrap());
    }

    #[test]
    fn an_https_source_without_a_key_still_says_the_extension_can_update() {
        let (_dir, root) = storage::test_root();
        let host = root.join("host");
        std::fs::write(&host, b"stub").unwrap();
        let config = session::Config {
            allowed_origins: Vec::new(),
            service_paths: Default::default(),
            update_source: Some("https://updates.example.com/channel".into()),
            update_public_key: None,
            extension_dir: None,
        };
        storage::write(&root.join("config.json"), &config).unwrap();

        let report = run(&root, &host);
        let check = find(&report, "update-source");
        assert!(check["ok"].as_bool().unwrap(), "{check}");
        let detail = check["detail"].as_str().unwrap();
        assert!(detail.contains("扩展"), "{detail}");
        assert!(detail.contains("签名公钥"), "{detail}");
    }

    #[test]
    fn an_extension_directory_without_a_readable_manifest_fails() {
        let (dir, root) = storage::test_root();
        let host = root.join("host");
        std::fs::write(&host, b"stub").unwrap();
        let extension_dir = dir.path().join("extension");
        std::fs::create_dir_all(&extension_dir).unwrap();
        let config = session::Config {
            allowed_origins: Vec::new(),
            service_paths: Default::default(),
            update_source: None,
            update_public_key: None,
            extension_dir: Some(extension_dir.to_string_lossy().into_owned()),
        };
        storage::write(&root.join("config.json"), &config).unwrap();
        assert!(!find(&run(&root, &host), "extension")["ok"]
            .as_bool()
            .unwrap());

        std::fs::write(
            extension_dir.join("manifest.json"),
            br#"{"manifest_version":3,"name":"x","version":"1.0.4"}"#,
        )
        .unwrap();
        let report = run(&root, &host);
        assert!(find(&report, "extension")["ok"].as_bool().unwrap());
        assert!(find(&report, "extension")["detail"]
            .as_str()
            .unwrap()
            .contains("1.0.4"));
    }
}
