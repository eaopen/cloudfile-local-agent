//! CloudFile Agent 2.2 desktop process.
//!
//! One resident process owns every task, lease and publication intent. The tray
//! menu, the draggable entry and the light notice are all views onto that same
//! durable store: they never hold their own copy of a decision.
//!
//! Exit policy: the process may only stop without a warning when nothing is at
//! risk. A held Checkout or an unconfirmed intent stops a silent exit, and the
//! user is asked from the same window they already use.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]
use cloudfile_core::{
    apps, auto_upload, autostart, device, exclusive, guard, hostreg, ipc, journal, native,
    paired_session, session, settings, storage, update, upload, wire,
};
use serde_json::{json, Value};
use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use tauri::{Emitter, Manager};

struct State {
    root: PathBuf,
    uploading: Arc<Mutex<HashSet<String>>>,
    quitting: Arc<AtomicBool>,
    toast: Arc<Mutex<u64>>,
    /// Live handle to the tray's status row. The tray is the only place the
    /// guard verdict is visible without opening a window, so the row is kept
    /// addressable instead of being rebuilt on every refresh.
    guard_row: Arc<Mutex<Option<tauri::menu::MenuItem<tauri::Wry>>>>,
    /// Coalesces the position writes produced by a single drag: the native drag
    /// emits one `Moved` per frame and only the resting place is worth storing.
    float_moves: Arc<Mutex<u64>>,
}

fn show(app: &tauri::AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
}

fn desktop_binary() -> PathBuf {
    std::env::current_exe().unwrap_or_else(|_| PathBuf::from("cloudfile-desktop"))
}

fn host_binary() -> Option<PathBuf> {
    let directory = std::env::current_exe().ok()?.parent()?.to_path_buf();
    let name = if cfg!(windows) {
        "cloudfile-native-host.exe"
    } else {
        "cloudfile-native-host"
    };
    let candidate = directory.join(name);
    candidate.is_file().then_some(candidate)
}

/// Rich view model for the task centre. Grouping is derived here so the UI can
/// stay a pure renderer.
fn summaries(root: &Path, uploading: &Mutex<HashSet<String>>) -> anyhow::Result<Value> {
    let mut tasks = Vec::new();
    let busy = uploading
        .lock()
        .map_err(|_| anyhow::anyhow!("上传状态不可用"))?
        .clone();
    for task in session::tasks(root)? {
        let changed = session::local_path(root, &task)
            .and_then(|path| session::digest(&path))
            .map(|digest| digest != task.baseline_digest);
        let pending = journal::pending(root, &task.task_id);
        let automatic = auto_upload::settings(root, &task.task_id);
        let lease = journal::lease(root, &task.task_id);
        let acknowledged = journal::lease_acknowledged(root, &task.task_id);
        let closed = journal::closed(root, &task.task_id);
        let exclusive_mode = task.mode == "exclusive-edit" && task.device_id.is_some();
        let action = pending.as_ref().ok().and_then(|value| {
            value.as_ref().map(|intent| {
                match intent.action {
                    journal::Action::OptimisticUpdate => "optimistic-update",
                    journal::Action::Commit => "commit",
                    journal::Action::Checkin => "checkin",
                }
                .to_string()
            })
        });
        let needs_review =
            changed.is_err() || pending.is_err() || automatic.is_err() || lease.is_err();
        let local_changed = changed.as_ref().copied().unwrap_or(false);
        let is_uploading = busy.contains(&task.task_id);
        let has_pending = pending.ok().flatten().is_some();
        let has_lease = lease.ok().flatten().is_some();
        let closed = closed.unwrap_or(false);
        // Three states the user can act on, in the order they matter.
        let group = if needs_review || has_pending || (exclusive_mode && !closed && !has_lease) {
            "needs-attention"
        } else if is_uploading || local_changed || (exclusive_mode && !closed) {
            "in-progress"
        } else {
            "done"
        };
        tasks.push(json!({"task_id":task.task_id,"file_name":task.file_name,
            "mode":native::mode_label(&task.mode),"internal_mode":task.mode,
            "service":task.server,"remote_path":task.remote_path,
            "group":group,
            "local_changed":local_changed,
            "needs_review":needs_review,
            "can_upload":task.mode == "optimistic-edit" && task.device_id.is_some(),
            "exclusive":exclusive_mode,
            "has_lease":has_lease,
            "lease_acknowledged":acknowledged.unwrap_or(false),
            "checkout_closed":closed,
            "auto_upload":automatic.ok(),"uploading":is_uploading,
            "pending_upload":has_pending,"pending_action":action}));
    }
    let count = |group: &str| tasks.iter().filter(|task| task["group"] == group).count();
    Ok(
        json!({"ok":true,"cdc_enabled":cloudfile_core::cdc::enabled(),"tasks":tasks,
        "counts":{"in_progress":count("in-progress"),"needs_attention":count("needs-attention"),"done":count("done")}}),
    )
}

#[tauri::command]
async fn task_summary(state: tauri::State<'_, State>) -> Result<Value, String> {
    let root = state.root.clone();
    let uploading = state.uploading.clone();
    tauri::async_runtime::spawn_blocking(move || {
        summaries(&root, &uploading).map_err(|_| "无法读取任务；请保留本地工作副本".to_string())
    })
    .await
    .map_err(|_| "任务执行失败".to_string())?
}

#[tauri::command]
fn settings_get(state: tauri::State<'_, State>) -> Result<Value, String> {
    let root = &state.root;
    let current = settings::load(root).map_err(|error| error.to_string())?;
    Ok(json!({
        "settings": current,
        "workspace_root": settings::workspace_root(root).to_string_lossy(),
        "autostart_enabled": autostart::enabled(),
        "install_root": update::install_root(&desktop_binary()).to_string_lossy(),
        "version": cloudfile_core::VERSION,
        "registered_host": hostreg::manifest_dir()
            .ok()
            .and_then(|dir| hostreg::state(&dir, hostreg::DEV_HOST, host_binary().as_deref()).ok())
            .map(|registration| json!({
                "host": registration.host,
                "registered": registration.binary.is_some(),
                "extension_id": registration.extension_id,
                "manifest_path": registration.manifest_path.to_string_lossy(),
                "foreign": registration.foreign,
            })),
    }))
}

#[tauri::command]
fn settings_set(
    state: tauri::State<'_, State>,
    notify_on_saved: Option<bool>,
    notify_on_review: Option<bool>,
    float_enabled: Option<bool>,
    float_hidden_when_empty: Option<bool>,
    workspace_root: Option<String>,
) -> Result<Value, String> {
    let updated = settings::update(&state.root, |current| {
        if let Some(value) = notify_on_saved {
            current.notify_on_saved = value;
        }
        if let Some(value) = notify_on_review {
            current.notify_on_review = value;
        }
        if let Some(value) = float_enabled {
            current.float_enabled = value;
        }
        if let Some(value) = float_hidden_when_empty {
            current.float_hidden_when_empty = value;
        }
        if let Some(value) = workspace_root.clone() {
            current.workspace_root = if value.trim().is_empty() {
                None
            } else {
                Some(value.trim().to_string())
            };
        }
    })
    .map_err(|error| error.to_string())?;
    Ok(json!(updated))
}

/// Monitor identity used to keep one resting place per display.
fn display_key(window: &tauri::WebviewWindow) -> String {
    window
        .current_monitor()
        .ok()
        .flatten()
        .and_then(|monitor| monitor.name().cloned())
        .unwrap_or_else(|| "unknown".to_string())
}

/// Stores where the entry came to rest, keyed by the display it landed on.
fn persist_float_position(app: &tauri::AppHandle) {
    let state = app.state::<State>();
    let Some(window) = app.get_webview_window("float") else {
        return;
    };
    let Ok(scale) = window.scale_factor() else {
        return;
    };
    let Ok(position) = window.outer_position() else {
        return;
    };
    let x = (position.x as f64 / scale).round() as i32;
    let y = (position.y as f64 / scale).round() as i32;
    // A failed write costs a remembered position, not a task: stay quiet.
    let _ = settings::set_position(&state.root, &display_key(&window), x, y);
}

/// Watches the native drag and saves only the final resting place. The drag is
/// performed by the OS, so the webview never reports pointer coordinates and
/// nothing here has to convert between logical and physical pixels.
fn watch_float_drag(app: &tauri::AppHandle) {
    let Some(window) = app.get_webview_window("float") else {
        return;
    };
    let handle = app.clone();
    window.on_window_event(move |event| {
        if !matches!(event, tauri::WindowEvent::Moved(_)) {
            return;
        }
        let state = handle.state::<State>();
        let Ok(generation) = state.float_moves.lock().map(|mut value| {
            *value += 1;
            *value
        }) else {
            return;
        };
        let handle = handle.clone();
        std::thread::spawn(move || {
            std::thread::sleep(std::time::Duration::from_millis(450));
            let state = handle.state::<State>();
            let Ok(current) = state.float_moves.lock().map(|value| *value) else {
                return;
            };
            if current == generation {
                persist_float_position(&handle);
            }
        });
    });
}

/// Shows or hides the entry according to the task list and the user's choice.
/// Called by the main window after every refresh so the two views cannot drift.
#[tauri::command]
async fn float_sync(
    app: tauri::AppHandle,
    state: tauri::State<'_, State>,
) -> Result<Value, String> {
    let root = state.root.clone();
    let uploading = state.uploading.clone();
    let summary = tauri::async_runtime::spawn_blocking(move || summaries(&root, &uploading))
        .await
        .map_err(|_| "任务执行失败".to_string())?
        .map_err(|_| "无法读取任务".to_string())?;
    let config = settings::load(&state.root).map_err(|error| error.to_string())?;
    let total = summary["tasks"].as_array().map(Vec::len).unwrap_or(0);
    let attention = summary["counts"]["needs_attention"].as_u64().unwrap_or(0);
    let Some(window) = app.get_webview_window("float") else {
        return Ok(json!({"visible": false}));
    };
    let visible = config.float_enabled && (total > 0 || !config.float_hidden_when_empty);
    let was_visible = window.is_visible().unwrap_or(false);
    if visible {
        if !was_visible {
            // Only restore on the hidden-to-shown transition, never during a drag.
            let display = display_key(&window);
            if let Ok(Some(saved)) = settings::position(&state.root, &display) {
                let _ = window
                    .set_position(tauri::LogicalPosition::new(saved.x as f64, saved.y as f64));
            }
            let _ = window.show();
        }
    } else if was_visible {
        let _ = window.hide();
    }
    let payload = json!({"total": total, "needs_attention": attention, "visible": visible});
    let _ = window.emit("float-state", payload.clone());
    Ok(payload)
}

#[tauri::command]
fn open_task_center(app: tauri::AppHandle) {
    show(&app);
}

/// Non-focusing notice. The window is only ever shown, never focused, and is
/// hidden again after a fixed interval unless a newer notice replaced it.
fn present_toast(app: &tauri::AppHandle, level: &str, text: &str) {
    let state = app.state::<State>();
    let Some(window) = app.get_webview_window("toast") else {
        return;
    };
    let text: String = text.chars().take(240).collect();
    let Ok(generation) = state.toast.lock().map(|mut value| {
        *value += 1;
        *value
    }) else {
        return;
    };
    let _ = window.set_ignore_cursor_events(true);
    if let Some(monitor) = window
        .current_monitor()
        .ok()
        .flatten()
        .or_else(|| window.primary_monitor().ok().flatten())
    {
        let size = monitor.size();
        let position = monitor.position();
        let width = 360i32;
        let x = position.x + size.width as i32 - width - 16;
        let y = position.y + 16;
        let _ = window.set_position(tauri::PhysicalPosition::new(x, y));
    }
    let _ = window.emit(
        "toast",
        json!({"level": if level == "warning" {"warning"} else {"info"}, "text": text}),
    );
    let _ = window.show();
    let handle = app.clone();
    std::thread::spawn(move || {
        std::thread::sleep(std::time::Duration::from_millis(3_500));
        let state = handle.state::<State>();
        let Ok(current) = state.toast.lock().map(|value| *value) else {
            return;
        };
        if current == generation {
            if let Some(window) = handle.get_webview_window("toast") {
                let _ = window.hide();
            }
        }
    });
}

#[tauri::command]
fn toast_show(app: tauri::AppHandle, level: String, text: String) {
    present_toast(&app, &level, &text);
}

#[tauri::command]
fn toast_hide(app: tauri::AppHandle) {
    if let Some(window) = app.get_webview_window("toast") {
        let _ = window.hide();
    }
}

/// Re-read from disk on every call. A cached answer here would be a lie after
/// the journal changed elsewhere.
#[tauri::command]
fn exit_assessment(state: tauri::State<'_, State>) -> Value {
    serde_json::to_value(guard::assess(&state.root))
        .unwrap_or_else(|_| json!({"safe_to_exit": false}))
}

#[tauri::command]
fn quit_app(app: tauri::AppHandle, state: tauri::State<'_, State>, force: bool) -> Value {
    let assessment = guard::assess(&state.root);
    if !force && !assessment.safe_to_exit {
        // Keep running, keep renewing, and ask from the window the user knows.
        show(&app);
        let _ = app.emit(
            "confirm-exit",
            serde_json::to_value(&assessment).unwrap_or(Value::Null),
        );
        return json!({"state": "blocked", "assessment": assessment});
    }
    state.quitting.store(true, Ordering::SeqCst);
    app.exit(0);
    json!({"state": "exiting"})
}

#[tauri::command]
async fn register_host(
    state: tauri::State<'_, State>,
    extension_id: String,
    takeover: bool,
) -> Result<Value, String> {
    let binary = host_binary().ok_or_else(|| "本机未找到 cloudfile-native-host".to_string())?;
    let dir = hostreg::manifest_dir().map_err(|error| error.to_string())?;
    let outcome = hostreg::install(&dir, hostreg::DEV_HOST, &extension_id, &binary, takeover)
        .map_err(|error| error.to_string())?;
    let _ = state.root;
    serde_json::to_value(outcome).map_err(|error| error.to_string())
}

#[tauri::command]
fn unregister_host() -> Result<(), String> {
    let dir = hostreg::manifest_dir().map_err(|error| error.to_string())?;
    hostreg::unregister(&dir, hostreg::DEV_HOST)
        .map(|_| ())
        .map_err(|error| error.to_string())
}

#[tauri::command]
fn rollback_host() -> Result<String, String> {
    let dir = hostreg::manifest_dir().map_err(|error| error.to_string())?;
    hostreg::rollback(&dir, hostreg::DEV_HOST)
        .map(|path| path.to_string_lossy().into_owned())
        .map_err(|error| error.to_string())
}

#[tauri::command]
fn autostart_set(state: tauri::State<'_, State>, enabled: bool) -> Result<Value, String> {
    if enabled {
        autostart::install(&state.root, &desktop_binary()).map_err(|error| error.to_string())?;
    } else {
        autostart::remove(&state.root).map_err(|error| error.to_string())?;
    }
    Ok(json!({"autostart_enabled": autostart::enabled()}))
}

#[tauri::command]
async fn check_update(state: tauri::State<'_, State>) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || native::check_update(&root))
        .await
        .map_err(|_| "更新检查失败".to_string())
}

/// Launches the upgrade in a detached process so the UI stays responsive and
/// the running build is only replaced by the staged one after a health check.
///
/// Two things stop an upgrade before it starts. The update channel must be
/// configured and pinned, otherwise there is nothing to verify against. And an
/// upgrade replaces the process that renews Checkouts and publishes intents, so
/// it is refused while anything is at risk unless the caller passes `force`
/// with the user's explicit consent — the same rule the exit path follows.
#[tauri::command]
fn run_update(state: tauri::State<'_, State>, force: bool) -> Result<Value, String> {
    let config = session::config(&state.root).map_err(|error| error.to_string())?;
    if config.update_source.is_none() || config.update_public_key.is_none() {
        return Err("尚未配置签名更新源；请在配置中设置更新地址与公钥后再升级".to_string());
    }
    let assessment = guard::assess(&state.root);
    if !assessment.safe_to_exit && !force {
        return Ok(json!({"state": "blocked", "assessment": assessment}));
    }
    let binary = host_binary().ok_or_else(|| "本机未找到 cloudfile-native-host".to_string())?;
    let mut command = std::process::Command::new(binary);
    command
        .arg("--update")
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        // 同 native-host：DETACHED_PROCESS 让升级进程脱离本窗口活下来，并且没有控制台
        // （所以这里不需要 `childproc::hide`）。
        command.creation_flags(0x0000_0008);
    }
    command
        .spawn()
        .map_err(|_| "无法启动更新进程".to_string())?;
    Ok(json!({"state": "running"}))
}

#[tauri::command]
fn open_workspace(state: tauri::State<'_, State>) -> Result<(), String> {
    apps::open_directory(&settings::workspace_root(&state.root)).map_err(|error| error.to_string())
}

#[tauri::command]
async fn save_cloud(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    let result = tauri::async_runtime::spawn_blocking(move || {
        let result = upload::save(&root, &task_id).map_err(|error| error.to_string())?;
        if result["state"] == "published" {
            let _ = auto_upload::confirmed(&root, &task_id);
        }
        Ok::<Value, String>(result)
    })
    .await
    .map_err(|_| "上传任务执行失败".to_string())??;
    Ok(result)
}

#[tauri::command]
async fn recover_upload(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let result = upload::recover(&root, &task_id).map_err(|error| error.to_string())?;
        if result["state"] == "published" {
            let _ = auto_upload::confirmed(&root, &task_id);
        }
        Ok::<Value, String>(result)
    })
    .await
    .map_err(|_| "查询任务执行失败".to_string())?
}

#[tauri::command]
async fn begin_pairing(state: tauri::State<'_, State>, origin: String) -> Result<(), String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        device::preflight(&root, &origin)
            .and_then(|()| device::begin(&root, &origin))
            .map_err(|error| error.to_string())
    })
    .await
    .map_err(|_| "设备连接失败".to_string())?
}

#[tauri::command]
fn service_status(state: tauri::State<'_, State>) -> Result<Value, String> {
    let origins = session::config(&state.root)
        .map_err(|_| "配置读取失败".to_string())?
        .allowed_origins;
    Ok(json!(origins
        .into_iter()
        .map(|origin| json!({"state":device::pairing_state(&state.root,&origin),
            "service_url":format!("{}{}",origin,session::service_path(&state.root,&origin).unwrap_or_default()),
            "origin":origin}))
        .collect::<Vec<_>>()))
}

#[tauri::command]
fn set_auto_upload(
    state: tauri::State<'_, State>,
    task_id: String,
    enabled: bool,
) -> Result<(), String> {
    auto_upload::set_enabled(&state.root, &task_id, enabled).map_err(|error| error.to_string())
}

#[tauri::command]
fn trust_origin(state: tauri::State<'_, State>, origin: String) -> Result<(), String> {
    session::trust(&state.root, origin)
        .map_err(|_| "请输入完整服务地址，例如 https://files.example.com".into())
}

#[tauri::command]
fn trusted_origins(state: tauri::State<'_, State>) -> Result<Vec<String>, String> {
    session::config(&state.root)
        .map(|config| config.allowed_origins)
        .map_err(|_| "配置读取失败".into())
}

#[tauri::command]
fn open_task(state: tauri::State<'_, State>, task_id: String) -> Result<(), String> {
    session::launch(&state.root, &task_id).map_err(|error| error.to_string())
}

/// Explicit user action. `checkin` publishes and releases the Checkout.
#[tauri::command]
async fn commit_task(
    state: tauri::State<'_, State>,
    task_id: String,
    checkin: bool,
) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::commit(&root, &task_id, checkin).map_err(|error| error.to_string())
    })
    .await
    .map_err(|_| "提交任务执行失败".to_string())?
}

/// Recovers the original commit intent; never creates a new one.
#[tauri::command]
async fn recover_commit(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::recover(&root, &task_id).map_err(|error| error.to_string())
    })
    .await
    .map_err(|_| "查询提交执行失败".to_string())?
}

#[tauri::command]
async fn renew_checkout(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::renew_checkout(&root, &task_id).map_err(|error| error.to_string())
    })
    .await
    .map_err(|_| "续租执行失败".to_string())?
}

/// Explicit release. The server refuses while a commit intent is pending.
#[tauri::command]
async fn release_checkout(
    state: tauri::State<'_, State>,
    task_id: String,
) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::release(&root, &task_id).map_err(|error| error.to_string())
    })
    .await
    .map_err(|_| "释放 Checkout 执行失败".to_string())?
}

/// Keeps the tray tooltip and the guard menu row honest about the real store.
/// `set_text` dispatches to the main thread on its own, so this is safe to call
/// from the background guard loop.
fn refresh_tray(state: &State, app: &tauri::AppHandle) {
    let assessment = guard::assess(&state.root);
    let summary = if assessment.safe_to_exit {
        "可以安全退出".to_string()
    } else {
        format!(
            "{} 个独占任务 · {} 项待确认",
            assessment.active_checkouts, assessment.uncertain
        )
    };
    if let Some(tray) = app.tray_by_id("main") {
        let _ = tray.set_tooltip(Some(format!("CloudFile Agent 2.2 · {summary}")));
    }
    if let Ok(row) = state.guard_row.lock() {
        if let Some(item) = row.as_ref() {
            let prefix = if assessment.safe_to_exit {
                "状态"
            } else {
                "注意"
            };
            let _ = item.set_text(format!("{prefix}：{summary}"));
        }
    }
}

const USAGE: &str = "\
CloudFile Agent 2.2 desktop

Usage: cloudfile-desktop [--autostart]

  --version     print the build version
  --component   print the local component contract as JSON
  --help, -h    print this message
  --autostart   start resident in the tray without opening the window; the login
                item passes this so logging in never steals focus
";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    // The upgrade health check starts a freshly staged build and reads its
    // stdout, so these answers must not depend on a window, the private store
    // or the single-instance lock being free.
    match args.first().map(String::as_str) {
        Some("--version") => {
            println!("{}", cloudfile_core::VERSION);
            return;
        }
        Some("--component") => {
            let value = json!({
                "version": cloudfile_core::VERSION,
                "ipc_version": update::IPC_VERSION,
                "journal_version": update::JOURNAL_VERSION,
                "agent_family": "agent2",
                "platform": update::platform_id(std::env::consts::OS, std::env::consts::ARCH),
                "role": "desktop",
            });
            println!(
                "{}",
                serde_json::to_string_pretty(&value).unwrap_or_default()
            );
            return;
        }
        Some("--help") | Some("-h") => {
            print!("{USAGE}");
            return;
        }
        Some(argument) if argument.starts_with('-') && argument != "--autostart" => {
            // Guessing at an unknown flag would start a second resident agent.
            eprintln!("unknown argument {argument}\n\n{USAGE}");
            std::process::exit(2);
        }
        _ => {}
    }
    let autostart = args.iter().any(|value| value == "--autostart");
    if let Err(error) = run(autostart) {
        eprintln!("CloudFile desktop: {error}");
    }
}

fn run(autostart: bool) -> anyhow::Result<()> {
    let root = storage::root()?;
    let _lock = match storage::lock(&root.join("desktop.lock")) {
        Ok(lock) => lock,
        // A second launch only needs to raise the window the first one owns.
        Err(_) => return Ok(()),
    };
    ipc::recover(&root)?;
    let uploading = Arc::new(Mutex::new(HashSet::new()));
    let quitting = Arc::new(AtomicBool::new(false));
    let state = State {
        root: root.clone(),
        uploading: uploading.clone(),
        quitting: quitting.clone(),
        toast: Arc::new(Mutex::new(0)),
        guard_row: Arc::new(Mutex::new(None)),
        float_moves: Arc::new(Mutex::new(0)),
    };
    tauri::Builder::default()
        .manage(state)
        .invoke_handler(tauri::generate_handler![
            task_summary,
            settings_get,
            settings_set,
            float_sync,
            open_task_center,
            toast_show,
            toast_hide,
            exit_assessment,
            quit_app,
            register_host,
            unregister_host,
            rollback_host,
            autostart_set,
            check_update,
            run_update,
            open_workspace,
            trust_origin,
            trusted_origins,
            open_task,
            save_cloud,
            recover_upload,
            begin_pairing,
            service_status,
            set_auto_upload,
            commit_task,
            recover_commit,
            renew_checkout,
            release_checkout
        ])
        .setup(move |app| {
            use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
            use tauri::tray::TrayIconBuilder;
            let open = MenuItem::with_id(app, "open", "打开任务中心", true, None::<&str>)?;
            let refresh = MenuItem::with_id(app, "refresh", "刷新任务", true, None::<&str>)?;
            let guard_row = MenuItem::with_id(app, "guard", "状态：检查中", false, None::<&str>)?;
            if let Ok(mut slot) = app.state::<State>().guard_row.lock() {
                *slot = Some(guard_row.clone());
            }
            let separator = PredefinedMenuItem::separator(app)?;
            let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&open, &refresh, &guard_row, &separator, &quit])?;
            let icon = tauri::image::Image::new_owned([30u8, 118, 238, 255].repeat(32 * 32), 32, 32);
            // The window doubles as the fallback entry when no tray is allowed.
            let tray = TrayIconBuilder::with_id("main")
                .icon(icon)
                .tooltip("CloudFile Agent 2.2")
                .menu(&menu)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "open" => show(app),
                    "refresh" => {
                        let _ = app.emit("tasks-changed", ());
                        show(app);
                    }
                    "quit" => {
                        let state = app.state::<State>();
                        let assessment = guard::assess(&state.root);
                        if assessment.safe_to_exit {
                            state.quitting.store(true, Ordering::SeqCst);
                            app.exit(0);
                        } else {
                            // Ask before stopping renewal; never stop silently.
                            show(app);
                            let _ = app.emit(
                                "confirm-exit",
                                serde_json::to_value(&assessment).unwrap_or(Value::Null),
                            );
                        }
                    }
                    _ => (),
                })
                .build(app);
            let has_tray = tray.is_ok();
            let window = app
                .get_webview_window("main")
                .ok_or_else(|| anyhow::anyhow!("main window missing"))?;
            if autostart && has_tray {
                // Started by the login item: stay resident and reachable from the
                // tray instead of opening a window on every login. Without a tray
                // there would be no way back, so the window is kept.
                let _ = window.hide();
            }
            let handle = app.handle().clone();
            window.on_window_event(move |event| {
                if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                    if has_tray {
                        api.prevent_close();
                        if let Some(window) = handle.get_webview_window("main") {
                            let _ = window.hide();
                        }
                    }
                }
            });
            for label in ["float", "toast"] {
                if let Some(extra) = app.get_webview_window(label) {
                    let _ = extra.set_always_on_top(true);
                    if label == "toast" {
                        let _ = extra.set_ignore_cursor_events(true);
                    }
                }
            }
            watch_float_drag(app.handle());
            let handle = app.handle().clone();
            let auto_root = root.clone();
            let auto_handle = handle.clone();
            let auto_busy = uploading.clone();
            let auto_notice = handle.clone();
            std::thread::spawn(move || {
                let started = std::time::Instant::now();
                let mut observer = auto_upload::Observer::default();
                loop {
                    let notices = Arc::new(Mutex::new(Vec::<(String, String)>::new()));
                    let writer = notices.clone();
                    let mut changed = false;
                    let result = observer.scan_with(
                        &auto_root,
                        started.elapsed().as_millis() as u64,
                        |id, digest| {
                            changed = true;
                            if let Ok(mut busy) = auto_busy.lock() {
                                busy.insert(id.to_string());
                            }
                            let _ = auto_handle.emit("tasks-changed", ());
                            let result = upload::save_observed(&auto_root, id, digest);
                            if let Ok(value) = &result {
                                let name = session::load(&auto_root, id)
                                    .map(|task| task.file_name)
                                    .unwrap_or_default();
                                // A cloud save is only ever announced after the
                                // server confirmed it.
                                let notice = match value["state"].as_str() {
                                    Some("published") => {
                                        ("info".to_string(), format!("{name}：已保存到云端"))
                                    }
                                    Some("unchanged") => {
                                        ("info".to_string(), format!("{name}：与云端基线一致"))
                                    }
                                    _ => (
                                        "warning".to_string(),
                                        format!("{name}：上传结果未确认，请在任务中心检查"),
                                    ),
                                };
                                if let Ok(mut list) = writer.lock() {
                                    list.push(notice);
                                }
                            }
                            if let Ok(mut busy) = auto_busy.lock() {
                                busy.remove(id);
                            }
                            result
                        },
                    );
                    if result.is_err() {
                        eprintln!("CloudFile automatic upload storage requires review");
                        let _ = auto_handle.emit("tasks-changed", ());
                    }
                    if changed {
                        let _ = auto_handle.emit("tasks-changed", ());
                    }
                    let preferences = settings::load(&auto_root).unwrap_or_default();
                    if let Ok(list) = notices.lock() {
                        for (level, text) in list.iter() {
                            let allowed = if level == "warning" {
                                preferences.notify_on_review
                            } else {
                                preferences.notify_on_saved
                            };
                            if allowed {
                                present_toast(&auto_notice, level, text);
                            }
                        }
                    }
                    std::thread::sleep(std::time::Duration::from_secs(2));
                }
            });
            let guard_handle = handle.clone();
            std::thread::spawn(move || loop {
                let state = guard_handle.state::<State>();
                refresh_tray(&state, &guard_handle);
                std::thread::sleep(std::time::Duration::from_secs(5));
            });
            std::thread::spawn(move || loop {
                let result = ipc::drain(&root, |request| match request {
                    wire::Request::Status {} => native::status(&root),
                    wire::Request::Tasks {} => {
                        summaries(&root, &uploading).unwrap_or_else(|_| wire::error("task storage unavailable"))
                    }
                    wire::Request::CheckUpdate {} => native::check_update(&root),
                    wire::Request::QueryLocalFile { repo_id, path, mode } => {
                        native::query_local_file(&root, &repo_id, &path, &mode)
                    }
                    wire::Request::QueryLocalFolder { repo_id, mode } => {
                        native::query_local_folder(&root, &repo_id, &mode)
                    }
                    wire::Request::OpenWorkspace { repo_id, path, mode } => {
                        native::open_workspace(&root, &repo_id, &path, &mode)
                    }
                    wire::Request::OpenSessionFile { path } => native::open_session_file(&path),
                    wire::Request::PairEnrollment { server } => match device::enrollment(&root, &server) {
                        Ok(value) => json!({"ok":true,"enrollment":value,
                            "service_path":session::service_path(&root,&server).unwrap_or_default()}),
                        Err(error) => wire::error(&error.to_string()),
                    },
                    wire::Request::PairSign { server, response } => {
                        match device::pair_sign(&root, &server, response) {
                            Ok(value) => json!({"ok":true,"proof":value}),
                            Err(error) => wire::error(&error.to_string()),
                        }
                    }
                    wire::Request::PairConfirm { server, response } => match device::confirm(&root, &server, response) {
                        Ok(()) => {
                            show(&handle);
                            json!({"ok":true})
                        }
                        Err(error) => wire::error(&error.to_string()),
                    },
                    wire::Request::OpenUri { uri } => match paired_session::open(&root, &uri) {
                        Ok(task) => {
                            show(&handle);
                            match session::launch(&root, &task.task_id) {
                                Ok(()) => json!({"ok":true}),
                                Err(_) => wire::error("文件已保留，未能打开本地应用，请在任务中心查看"),
                            }
                        }
                        Err(error) => wire::error(&error.to_string()),
                    },
                    wire::Request::Focus { task_id } => {
                        if session::load(&root, &task_id).is_ok() {
                            show(&handle);
                            let _ = handle.emit("focus-task", task_id);
                            json!({"ok":true})
                        } else {
                            wire::error("task unavailable")
                        }
                    }
                    wire::Request::Update {} | wire::Request::UpdateExtension {} => {
                        wire::error("请在任务中心或命令行执行更新")
                    }
                    request @ wire::Request::Open { .. } => match session::open_session(&root, request) {
                        Ok(task) => {
                            show(&handle);
                            match session::launch(&root, &task.task_id) {
                                Ok(()) => json!({"ok":true}),
                                Err(_) => wire::error("download retained; local application could not open file"),
                            }
                        }
                        Err(_) => wire::error("session rejected or download failed; inspect trusted origin and server session"),
                    },
                });
                if result.is_err() {
                    eprintln!("CloudFile mailbox operation failed");
                }
                std::thread::sleep(std::time::Duration::from_millis(150));
            });
            Ok(())
        })
        .run(tauri::generate_context!())?;
    Ok(())
}
