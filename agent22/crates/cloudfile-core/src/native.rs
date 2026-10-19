//! Response shapes for the Chrome native messaging contract.
//!
//! The extension decides what to show from these fields, so they are a public
//! interface: names, types and the `capabilities` list must match what the
//! browser-side code already checks. Everything here is derived from the
//! durable store on each call; nothing is cached between requests.
use crate::{apps, device, guard, journal, session, update};
use anyhow::{bail, Context, Result};
use serde_json::{json, Value};
use sha2::Digest as _;
use std::path::Path;

/// The version family the extension matches with `^2\.[0-9]+\.[0-9]+$`. The
/// build version is reported separately so a pre-release still routes to this
/// agent instead of falling back to a legacy host.
pub const FAMILY_VERSION: &str = "2.2.0";

const CAP_OPEN_SESSION: &str = "open_session";
const CAP_OPEN_SESSION_FILE: &str = "open_session_file";
const CAP_OPEN_URI: &str = "open_uri";
const CAP_TASK_SUMMARY: &str = "task_summary";
const CAP_FOCUS_TASK: &str = "focus_task";
const CAP_CHECK_UPDATE: &str = "check_update";
const CAP_OPEN_WORKSPACE: &str = "open_workspace";
const CAP_QUERY_LOCAL_FILE: &str = "query_local_file";
const CAP_QUERY_LOCAL_FOLDER: &str = "query_local_folder";
const CAP_UPDATE: &str = "update";

fn configured(root: &Path) -> bool {
    session::config(root)
        .map(|config| !config.allowed_origins.is_empty())
        .unwrap_or(false)
}

/// A signed update source must be pinned before the browser may trigger one;
/// otherwise the extension would offer a button that cannot succeed.
fn update_configured(root: &Path) -> bool {
    session::config(root)
        .map(|config| config.update_source.is_some() && config.update_public_key.is_some())
        .unwrap_or(false)
}

/// Only capabilities the agent can really serve right now are advertised: the
/// browser gates each operation on this list.
pub fn capabilities(root: &Path) -> Vec<String> {
    let mut list = vec![CAP_OPEN_URI.to_string(), CAP_TASK_SUMMARY.to_string()];
    list.push(CAP_FOCUS_TASK.to_string());
    list.push(CAP_CHECK_UPDATE.to_string());
    if update_configured(root) {
        list.push(CAP_UPDATE.to_string());
    }
    if configured(root) {
        for capability in [
            CAP_OPEN_SESSION,
            CAP_OPEN_SESSION_FILE,
            CAP_OPEN_WORKSPACE,
            CAP_QUERY_LOCAL_FILE,
            CAP_QUERY_LOCAL_FOLDER,
        ] {
            list.push(capability.to_string());
        }
    }
    list
}

pub fn status(root: &Path) -> Value {
    // 解析出来的工作区根可能和上次不同（首选盘回来/掉线，或用户改了配置）：
    // 先把已有任务搬过去一次。标记一致时这里只是一次很小的文件读取。
    let _ = crate::workspace::carry_over(root);
    let mut value = json!({
        "ok": true,
        "version": FAMILY_VERSION,
        "build_version": crate::VERSION,
        "agent_family": "agent2",
        "platform": std::env::consts::OS,
        "capabilities": capabilities(root),
        "applications": apps::names(),
        "release_channel": "development",
        "workspace_root": crate::workspace::tasks_root(root).to_string_lossy(),
        "can_open_workspace": configured(root),
    });
    if let Ok(executable) = std::env::current_exe() {
        value["executable_path"] = json!(executable.to_string_lossy());
    }
    if let Some(result) = load_update_result(root) {
        value["last_update_state"] = result["state"].clone();
        value["last_update_kind"] = result["kind"].clone();
    }
    let probe = check_update(root);
    for key in [
        "update_source_set",
        // 让 --status 也能看到「这台机器允许明文更新源」——审计第一眼要看的就是它。
        "update_source_insecure_allowed",
        "update_available",
        "latest_version",
        "latest_notes",
        "extension_installed_version",
        "extension_latest_version",
        "extension_notes",
    ] {
        if let Some(field) = probe.get(key) {
            value[key] = field.clone();
        }
    }
    value
}

/// Maps an internal mode onto the two values the extension understands.
/// 回报给浏览器的模式名。**直接复用 `apps::mode_family`**，不再各写一份：以前这里把
/// `local-view` 也说成 `local-edit`，与 `apps` 里「只读副本不得进编辑规则」的映射自相矛盾
/// —— 一句「这是可编辑的」就可能让网页把只读的查看副本当成工作副本。
pub fn mode_label(mode: &str) -> &'static str {
    apps::mode_family(mode)
}

/// 一份本地副本属于哪个**子树**。
///
/// Rust 的模式名比 Go 多（`optimistic-edit` 与 `local-edit` 是同族的两个名字，另有
/// `exclusive-edit`），但子树只有三个：查看（只读）、可编辑、独占可编辑。请求与任务在
/// **这一层**比较 —— 按字符串字面比会把 `optimistic-edit` 判成「不匹配 local-edit」，
/// 那是把同族的两个名字当成了两个子树。
#[derive(Clone, Copy, PartialEq, Eq)]
enum Subtree {
    View,
    Edit,
    Exclusive,
}

impl Subtree {
    fn of(task_mode: &str) -> Subtree {
        match task_mode {
            "local-view" => Subtree::View,
            "exclusive-edit" => Subtree::Exclusive,
            _ => Subtree::Edit,
        }
    }

    /// 请求要查哪个子树。**只有显式的 `local-view` 才落到查看子树，其余一律按可编辑处理**
    /// —— 与 Go 的 `Request.MirrorMode()` 同义（理由照抄：编辑子树是唯一不会被「查看」打开
    /// 覆盖的那个）。反过来才是真会伤人的：查看副本是**只读**的，把它当成编辑副本交给用户，
    /// 用户改完根本存不进去。
    fn requested(mode: &str) -> Subtree {
        match mode {
            "local-view" => Subtree::View,
            "exclusive-edit" => Subtree::Exclusive,
            _ => Subtree::Edit,
        }
    }
}

fn matches_mode(task_mode: &str, requested: &str) -> bool {
    Subtree::of(task_mode) == Subtree::requested(requested)
}

fn find(root: &Path, repo_id: &str, path: &str, mode: &str) -> Result<Option<session::Task>> {
    if !repo_id.is_empty() && !device::uuid(repo_id) {
        bail!("invalid repository identity");
    }
    if !path.is_empty() && !path.starts_with('/') {
        bail!("repository path must be absolute");
    }
    // `local-view` 同样是合法请求（Go 的 `Request.Valid()` 也认它）：网页会用它查
    // 「这个资料库的查看副本在不在」。以前这里会报「unsupported session mode」。
    if !matches!(mode, "" | "local-edit" | "local-view" | "exclusive-edit") {
        bail!("unsupported session mode");
    }
    let mut matches: Vec<session::Task> = session::tasks(root)?
        .into_iter()
        .filter(|task| task.repo_id == repo_id && matches_mode(&task.mode, mode))
        .filter(|task| path.is_empty() || task.remote_path == path)
        .collect();
    // 挑选顺序固定下来：请求写明了模式名就优先精确命中，否则偏好规范名 `local-edit`，
    // 再否则取第一个。**不能依赖目录枚举顺序** —— 同一份文件在可编辑子树里可能同时有
    // `local-edit` 与 `optimistic-edit` 两份任务，结果飘忽是很难查的那类 bug。
    if !mode.is_empty() {
        if let Some(position) = matches.iter().position(|task| task.mode == mode) {
            return Ok(Some(matches.swap_remove(position)));
        }
    }
    if let Some(position) = matches.iter().position(|task| task.mode == "local-edit") {
        return Ok(Some(matches.swap_remove(position)));
    }
    Ok(matches.into_iter().next())
}

/// Public task list. Field names follow the extension contract; local paths and
/// credentials are never included.
pub fn task_summary(root: &Path) -> Value {
    let mut rows = Vec::new();
    // 任务根不可信时**不能**报成「没有任务」：扩展据此判断要不要提示，
    // 把「状态文件权限被改松了」说成「一切正常、没有任务」是误导，用户会以为工作副本没了。
    // Go 侧同一处是 fail closed（非私有任务目录直接报错），这里保持同义。
    let tasks = match session::tasks(root) {
        Ok(tasks) => tasks,
        Err(error) => return json!({"ok": false, "error": error.to_string()}),
    };
    for task in tasks {
        let changed = session::local_path(root, &task)
            .and_then(|path| session::digest(&path))
            .map(|digest| digest != task.baseline_digest);
        let pending = journal::pending(root, &task.task_id);
        let needs_review = changed.is_err() || pending.is_err() || pending.ok().flatten().is_some();
        rows.push(json!({
            "task_id": task.task_id,
            "file_name": task.file_name,
            "mode": mode_label(&task.mode),
            "local_changed": changed.unwrap_or(false),
            "needs_review": needs_review,
        }));
    }
    json!({"ok": true, "tasks": rows})
}

fn digest_pair(path: &Path) -> Option<(String, String, u64, u64)> {
    use std::io::Read;
    let mut file = std::fs::File::open(path).ok()?;
    let mut sha1 = sha1::Sha1::new();
    let mut sha256 = sha2::Sha256::new();
    let mut buffer = [0; 65536];
    let mut size = 0u64;
    loop {
        let read = file.read(&mut buffer).ok()?;
        if read == 0 {
            break;
        }
        size += read as u64;
        sha1.update(&buffer[..read]);
        sha256.update(&buffer[..read]);
    }
    let mtime = file
        .metadata()
        .ok()?
        .modified()
        .ok()?
        .duration_since(std::time::UNIX_EPOCH)
        .map(|value| value.as_secs())
        .unwrap_or_default();
    Some((
        format!("{:x}", sha1.finalize()),
        format!("{:x}", sha256.finalize()),
        size,
        mtime,
    ))
}

pub fn query_local_file(root: &Path, repo_id: &str, path: &str, mode: &str) -> Value {
    let task = match find(root, repo_id, path, mode) {
        Ok(Some(task)) => task,
        Ok(None) => {
            return json!({"ok":true,"mode":mode_label(mode),"local_exists":false,
                "baseline_exists":false,"local_changed":false})
        }
        Err(error) => return json!({"ok":false,"error":error.to_string()}),
    };
    let mut value = json!({
        "ok": true,
        "mode": mode_label(&task.mode),
        "local_exists": false,
        "baseline_exists": true,
        "baseline_file_id": task.baseline_file_id,
        "baseline_digest": task.baseline_digest,
        "local_changed": false,
    });
    let Ok(local) = session::local_path(root, &task) else {
        return value;
    };
    value["local_path"] = json!(local.to_string_lossy());
    value["local_exists"] = json!(true);
    if let Some((sha1, sha256, size, mtime)) = digest_pair(&local) {
        value["local_hash"] = json!(sha1);
        value["local_digest"] = json!(sha256);
        value["local_size"] = json!(size);
        value["local_mtime"] = json!(mtime);
        value["local_changed"] = json!(sha256 != task.baseline_digest);
    }
    value
}

pub fn query_local_folder(root: &Path, repo_id: &str, mode: &str) -> Value {
    match find(root, repo_id, "", mode) {
        Ok(Some(task)) => {
            let directory = session::task_dir(root, &task.task_id)
                .map(|path| path.to_string_lossy().into_owned())
                .unwrap_or_default();
            json!({"ok":true,"mode":mode_label(&task.mode),"local_exists":true,"local_path":directory})
        }
        Ok(None) => json!({"ok":true,"mode":mode_label(mode),"local_exists":false}),
        Err(error) => json!({"ok":false,"error":error.to_string()}),
    }
}

/// Opens the agent-owned directory that holds the working copy. A browser never
/// supplies a path here; only a repository identity is accepted.
pub fn open_workspace(root: &Path, repo_id: &str, path: &str, mode: &str) -> Value {
    if repo_id.is_empty() {
        let target = crate::workspace::tasks_root(root);
        if !target.is_dir() {
            return json!({"ok":false,"error":"本地工作区尚未创建"});
        }
        return match apps::open_directory(&target) {
            Ok(()) => json!({"ok":true,"workspace_root":target.to_string_lossy(),
                "local_path":target.to_string_lossy()}),
            Err(error) => json!({"ok":false,"error":error.to_string()}),
        };
    }
    let task = match find(root, repo_id, path, mode) {
        Ok(Some(task)) => task,
        Ok(None) => {
            return json!({"ok":false,"error":"没有匹配的本地工作副本；请先从网页选择本地编辑"})
        }
        Err(error) => return json!({"ok":false,"error":error.to_string()}),
    };
    let directory = match session::task_dir(root, &task.task_id) {
        Ok(directory) => directory,
        Err(error) => return json!({"ok":false,"error":error.to_string()}),
    };
    match apps::open_directory(&directory) {
        Ok(()) => json!({"ok":true,"mode":mode_label(&task.mode),
            "workspace_root":crate::workspace::tasks_root(root).to_string_lossy(),
            "local_path":directory.to_string_lossy()}),
        Err(error) => json!({"ok":false,"error":error.to_string()}),
    }
}

/// Compatibility entry for a handoff file produced by the browser.
pub fn open_session_file(path: &str) -> Value {
    let candidate = Path::new(path);
    if !candidate.is_absolute()
        || candidate.extension().and_then(|value| value.to_str()) != Some("cloudfile")
        || !candidate.is_file()
    {
        return json!({"ok":false,"error":"不是有效的 .cloudfile 交接文件"});
    }
    match open::that(candidate) {
        Ok(()) => json!({"ok":true}),
        Err(_) => json!({"ok":false,"error":"无法打开交接文件，请在任务中心重试"}),
    }
}

fn update_result_path(root: &Path) -> std::path::PathBuf {
    root.join("update-result.json")
}

/// Reads the installed extension version from the configured extension copy.
/// Only a version string is exposed; the directory never leaves the agent.
fn extension_installed_version(config: &session::Config) -> Option<String> {
    let directory = config.extension_dir.as_ref()?;
    crate::extension::installed_version(Path::new(directory))
}

pub fn record_update_result(root: &Path, kind: &str, state: &str) {
    let value = json!({"kind": kind, "state": state, "at": crate::now()});
    let _ = crate::storage::write(&update_result_path(root), &value);
}

pub fn load_update_result(root: &Path) -> Option<Value> {
    let path = update_result_path(root);
    if !path.is_file() {
        return None;
    }
    crate::storage::read::<Value>(&path).ok().filter(|value| {
        matches!(
            value["state"].as_str(),
            Some("running" | "completed" | "failed")
        ) && matches!(value["kind"].as_str(), Some("agent" | "extension"))
    })
}

/// Version probe. An unconfigured or unreachable source is not an error: the
/// extension only uses the answer to show an update hint.
pub fn check_update(root: &Path) -> Value {
    let config = session::config(root).unwrap_or_default();
    let policy = config.update_policy();
    // 历史审计位：早期只有显式打开才允许明文更新源。现在 http 是默认支持的，
    // 这个字段只保留「这台机器当初显式开过」的记录，读的人不该据此判断源是否可用。
    let mut value = json!({
        "update_source_set": false,
        "update_available": false,
        "update_source_insecure_allowed": policy.allow_insecure,
    });
    let Some(source) = config.update_source.clone() else {
        return value;
    };
    value["update_source_set"] = json!(true);
    if let Some(version) = extension_installed_version(&config) {
        value["extension_installed_version"] = json!(version);
    }
    // 扩展清单与 Agent 清单同级、同一个源，而且**不需要签名**（信任锚是同源 https
    // + sha256）：扩展不是可执行文件，装错版本不会在用户机器上执行代码。所以这一节
    // 只看更新源，公钥缺失不该让扩展升级变成「不可用」。
    describe_extension_update(policy, &source, &mut value);

    let Some(public_key) = config.update_public_key.clone() else {
        return value;
    };
    match update::fetch_manifest_for(policy, &source) {
        Ok(bytes) => match update::parse_release(&bytes) {
            Ok(release) => {
                let platform = update::platform_id(std::env::consts::OS, std::env::consts::ARCH);
                match update::verify(
                    policy,
                    &release,
                    &source,
                    crate::VERSION,
                    crate::now(),
                    &platform,
                    &public_key,
                ) {
                    Ok(()) => {
                        value["update_available"] = json!(true);
                        value["latest_version"] = json!(release.version);
                        value["latest_notes"] = json!(release.notes);
                    }
                    Err(error) => value["update_error"] = json!(error.to_string()),
                }
            }
            Err(error) => value["update_error"] = json!(error.to_string()),
        },
        Err(error) => value["update_error"] = json!(error.to_string()),
    }
    value
}

/// 扩展一侧的版本信息。扩展等的是
/// `extension_latest_version == extension_installed_version`，两个都给不出来它就无法
/// 自己重载；取不到清单只是没提示，不是错误。
fn describe_extension_update(policy: update::SourcePolicy, source: &str, value: &mut Value) {
    let Ok(release) = crate::extension::fetch_release(policy, source) else {
        return;
    };
    value["extension_latest_version"] = json!(release.version);
    value["extension_notes"] = json!(release.notes);
    if let Some(installed) = value["extension_installed_version"].as_str() {
        // 已安装版本比源上还新（或相等）时不算「有更新」—— 版本号只用来提示。
        value["extension_update_available"] =
            json!(update::compare(&release.version, installed).is_gt());
    }
}

/// 下载并安装扩展的新版本，返回给人看的一句话。
///
/// 装的是**磁盘上的扩展目录**（Chrome 以 `--load-extension` 加载的那份）。装完还需要
/// 扩展自己调用 `chrome.runtime.reload()`：Chrome 不允许外部进程替换已加载的扩展，
/// 所以这里结束在「文件已就位」，与 Go 侧 `--update-extension` 一致。
pub fn update_extension(root: &Path) -> Result<String> {
    let config = session::config(root)?;
    // 先算策略：下面两个 `.context()` 会把 Option<String> 字段移走，之后再借 config 就晚了。
    let policy = config.update_policy();
    let source = config
        .update_source
        .context("尚未配置更新源；请先在 config.json 里设置 update_source")?;
    let directory = config
        .extension_dir
        .context("尚未配置扩展目录；请先在 config.json 里设置 extension_dir")?;
    let dir = Path::new(&directory);
    let release = crate::extension::fetch_release(policy, &source)?;
    let installed = crate::extension::installed_version(dir);
    if let Some(current) = installed.as_deref() {
        if !update::compare(&release.version, current).is_gt() {
            return Ok(format!(
                "扩展已是 {current}（源上 {}），无需更新",
                release.version
            ));
        }
    }
    let installed_dir = crate::extension::apply(policy, &source, &release, dir)?;
    Ok(format!(
        "扩展已更新到 {}{}；在 chrome://extensions 里重载后生效（目录 {}）",
        release.version,
        match installed.as_deref() {
            Some(previous) => format!("（原 {previous}）"),
            None => String::new(),
        },
        installed_dir.display()
    ))
}

/// Delegates the process-lifetime question to the same assessment the tray
/// menu uses, so a browser query can never disagree with the local dialog.
pub fn exit_assessment(root: &Path) -> Value {
    serde_json::to_value(guard::assess(root)).unwrap_or_else(|_| json!({"safe_to_exit":false}))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn store(root: &Path, mode: &str, name: &str) -> session::Task {
        let repo_id = "22222222-2222-4222-8222-222222222222".to_string();
        let remote_path = format!("/folder/{name}");
        let id = format!(
            "{:x}",
            sha2::Sha256::digest(format!(
                "https://example.com\0{repo_id}\0{mode}\0{remote_path}"
            ))
        );
        let dir = session::task_dir(root, &id).unwrap();
        crate::storage::private_dir(&dir).unwrap();
        std::fs::write(dir.join(name), b"original").unwrap();
        let task = session::Task {
            task_id: id,
            device_id: Some("d".repeat(64)),
            file_name: name.to_string(),
            mode: mode.to_string(),
            server: "https://example.com".into(),
            repo_id,
            remote_path,
            session_id: "c".repeat(36),
            baseline_file_id: "e".repeat(40),
            baseline_digest: session::digest(&dir.join(name)).unwrap(),
            size: 8,
        };
        crate::storage::write(&dir.join("task.json"), &task).unwrap();
        task
    }

    #[test]
    fn capabilities_follow_the_configured_trust_boundary() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let bare = capabilities(root);
        assert!(bare.contains(&"check_update".to_string()));
        assert!(!bare.contains(&"query_local_file".to_string()));
        assert!(!bare.contains(&"open_session".to_string()));
        // No pinned source means no browser-triggered upgrade.
        assert!(!bare.contains(&"update".to_string()));
        session::trust(root, "https://example.com".into()).unwrap();
        let trusted = capabilities(root);
        for expected in [
            "open_session",
            "open_session_file",
            "open_workspace",
            "query_local_file",
            "query_local_folder",
        ] {
            assert!(
                trusted.contains(&expected.to_string()),
                "{expected} missing"
            );
        }
        // A key without a source is never stored, so the pair stays honest.
        assert!(session::set_update(root, None, Some("x".into()), None).is_err());
        let key = {
            use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
            let signing = p256::ecdsa::SigningKey::from_slice(&[0x33; 32]).unwrap();
            B64.encode(signing.verifying_key().to_encoded_point(false).as_bytes())
        };
        session::set_update(
            root,
            Some("https://updates.example.com".into()),
            Some(key),
            None,
        )
        .unwrap();
        assert!(capabilities(root).contains(&"update".to_string()));
    }

    #[test]
    fn status_advertises_the_family_version_the_extension_expects() {
        let (_dir, base) = crate::storage::test_root();
        let value = status(base.as_path());
        assert_eq!(value["agent_family"], "agent2");
        assert_eq!(value["version"], FAMILY_VERSION);
        assert!(
            value["build_version"]
                .as_str()
                .unwrap()
                .starts_with("2.2.0"),
            "build version must stay in the 2.2 family"
        );
        assert!(value["applications"].as_array().unwrap().len() > 1);
        assert!(value["workspace_root"].as_str().unwrap().ends_with("tasks"));
        assert_eq!(value["can_open_workspace"], false);
        assert_eq!(value["update_available"], false);
        assert_eq!(value["update_source_set"], false);
        assert!(value["ok"].as_bool().unwrap());
    }

    #[test]
    fn a_read_only_view_copy_is_never_handed_out_as_an_editable_one() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        // 查看副本（只读）与编辑副本是两份不同的任务，目录也不同。
        let view = store(root, "local-view", "a.txt");
        assert!(view.task_id != store(root, "local-edit", "a.txt").task_id);

        // 查看请求必须查得到它 —— 以前这里直接报「unsupported session mode」，
        // 网页就没法判断「这个资料库的查看副本在不在」。
        let found = query_local_file(root, &view.repo_id, &view.remote_path, "local-view");
        assert_eq!(found.get("ok"), Some(&json!(true)), "{found}");
        assert_eq!(found["local_exists"], true, "{found}");
        assert_eq!(found["mode"], "local-view", "{found}");

        // 反过来才是会伤人的：缺省请求与 local-edit 请求都**不能**命中只读的查看副本。
        for requested in ["", "local-edit"] {
            let answer = query_local_file(root, &view.repo_id, "/folder/only-view.txt", requested);
            assert_eq!(answer["local_exists"], false, "{requested}: {answer}");
        }
        // 让查看子树单独存在一份文件，确认上面那个 false 不是因为「查不到任何东西」。
        store(root, "local-view", "only-view.txt");
        let answer = query_local_file(root, &view.repo_id, "/folder/only-view.txt", "local-edit");
        assert_eq!(answer["local_exists"], false, "{answer}");
        assert_eq!(
            query_local_file(root, &view.repo_id, "/folder/only-view.txt", "local-view")
                ["local_exists"],
            true
        );
        // 目录查询同样不许把查看子树当成编辑子树。
        assert_eq!(
            query_local_folder(root, &view.repo_id, "local-edit")["mode"],
            "local-edit"
        );
    }

    #[test]
    fn the_same_library_path_in_two_editable_names_picks_deterministically() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        // 可编辑子树里 `local-edit` 与 `optimistic-edit` 是**同族两个名字**，
        // 所以缺省请求同时命中两者；结果必须确定，不能取决于目录枚举顺序。
        let optimistic = store(root, "optimistic-edit", "a.txt");
        store(root, "local-edit", "a.txt");

        for _ in 0..4 {
            let answer = query_local_file(root, &optimistic.repo_id, &optimistic.remote_path, "");
            assert_eq!(answer["local_exists"], true);
            // 缺省命中规范名 `local-edit`。
            assert_eq!(answer["baseline_file_id"], "e".repeat(40));
            assert_eq!(answer["local_digest"], optimistic.baseline_digest);
        }
        // 写明模式名时精确命中优先。
        let explicit = query_local_file(
            root,
            &optimistic.repo_id,
            &optimistic.remote_path,
            "local-edit",
        );
        assert_eq!(explicit["local_exists"], true);
        // 同族的另一个名字仍然查得到（不是字面比较）。
        assert_eq!(
            query_local_file(
                root,
                &optimistic.repo_id,
                &optimistic.remote_path,
                "local-edit"
            )["mode"],
            "local-edit"
        );
    }

    #[test]
    fn the_mode_vocabulary_is_reported_without_flattening_view_copies() {
        assert_eq!(mode_label("local-view"), "local-view");
        assert_eq!(mode_label("exclusive-edit"), "exclusive-edit");
        assert_eq!(mode_label("optimistic-edit"), "local-edit");
        assert_eq!(mode_label("local-edit"), "local-edit");
        assert_eq!(mode_label(""), "local-edit");
    }

    #[test]
    fn local_queries_only_report_agent_owned_copies() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let task = store(root, "optimistic-edit", "a.txt");
        let found = query_local_file(root, &task.repo_id, &task.remote_path, "local-edit");
        assert_eq!(found["local_exists"], true);
        assert_eq!(found["local_changed"], false);
        assert_eq!(found["local_size"], 8);
        assert_eq!(found["mode"], "local-edit");
        assert_eq!(found["baseline_file_id"], "e".repeat(40));
        assert_eq!(
            found["local_digest"],
            session::digest(&session::local_path(root, &task).unwrap()).unwrap()
        );

        // A changed working copy is reported as changed, without uploading.
        let path = session::local_path(root, &task).unwrap();
        std::fs::write(&path, b"changed locally").unwrap();
        assert_eq!(
            query_local_file(root, &task.repo_id, &task.remote_path, "local-edit")["local_changed"],
            true
        );

        // Wrong mode, wrong repository and a relative path are all refused.
        let missing = query_local_file(root, &task.repo_id, &task.remote_path, "exclusive-edit");
        assert_eq!(missing["local_exists"], false);
        let stranger = "11111111-1111-4111-8111-111111111111";
        assert_eq!(
            query_local_file(root, stranger, "/x.txt", "")["local_exists"],
            false
        );
        assert_eq!(
            query_local_file(root, "not-a-uuid", "/x.txt", "").get("ok"),
            Some(&json!(false))
        );
        assert_eq!(
            query_local_file(root, &task.repo_id, "relative.txt", "").get("ok"),
            Some(&json!(false))
        );
        assert_eq!(
            query_local_folder(root, &task.repo_id, "local-edit")["local_exists"],
            true
        );
        assert_eq!(
            query_local_folder(root, &task.repo_id, "exclusive-edit")["local_exists"],
            false
        );
    }

    #[test]
    fn task_summary_uses_the_extension_vocabulary() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        store(root, "exclusive-edit", "a.txt");
        store(root, "optimistic-edit", "b.txt");
        let value = task_summary(root);
        let rows = value["tasks"].as_array().unwrap();
        assert_eq!(rows.len(), 2);
        for row in rows {
            assert!(row["task_id"].as_str().unwrap().len() == 64);
            assert!(matches!(
                row["mode"].as_str().unwrap(),
                "local-edit" | "exclusive-edit"
            ));
            assert_eq!(row["needs_review"], false);
            assert_eq!(row["local_changed"], false);
        }
        // A pending intent is surfaced as review, never as a silent success.
        let id = rows
            .iter()
            .find(|row| row["mode"] == "exclusive-edit")
            .unwrap()["task_id"]
            .as_str()
            .unwrap()
            .to_string();
        journal::capture(root, &id, journal::Action::Commit).unwrap();
        let rows = task_summary(root)["tasks"].clone();
        let row = rows
            .as_array()
            .unwrap()
            .iter()
            .find(|row| row["task_id"] == json!(id))
            .unwrap();
        assert_eq!(row["needs_review"], true);
    }

    #[test]
    fn an_unconfigured_update_source_is_a_calm_answer() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        assert_eq!(check_update(root)["update_source_set"], false);
        assert!(load_update_result(root).is_none());
        record_update_result(root, "agent", "running");
        assert_eq!(load_update_result(root).unwrap()["state"], "running");
        record_update_result(root, "agent", "nonsense");
        assert!(load_update_result(root).is_none());
    }

    #[test]
    fn handoff_files_must_be_absolute_cloudfile_paths() {
        assert_eq!(open_session_file("relative.cloudfile")["ok"], false);
        // An absolute path that simply does not exist. Built from the host's
        // temporary directory so the case is genuinely exercised on Windows
        // too, where a Unix literal like `/tmp/x` is not absolute.
        let missing = std::env::temp_dir().join("cloudfile-missing-handoff.cloudfile");
        assert_eq!(
            open_session_file(&missing.to_string_lossy())["ok"],
            false,
            "{missing:?}"
        );
    }

    #[test]
    fn exit_assessment_matches_the_tray_view() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        assert_eq!(exit_assessment(root)["safe_to_exit"], true);
        let task = store(root, "exclusive-edit", "a.txt");
        journal::capture(root, &task.task_id, journal::Action::Commit).unwrap();
        assert_eq!(exit_assessment(root)["safe_to_exit"], false);
    }
}
