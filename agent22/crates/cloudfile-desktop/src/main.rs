#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]
use cloudfile_core::{
    auto_upload, device, exclusive, ipc, journal, paired_session, session, storage, upload, wire,
};
use serde_json::{json, Value};
use std::path::PathBuf;
use std::{
    collections::HashSet,
    sync::{Arc, Mutex},
};
use tauri::Manager;
struct State {
    root: PathBuf,
    uploading: Arc<Mutex<HashSet<String>>>,
}
fn show(app: &tauri::AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
    }
}
fn summaries(root: &std::path::Path, uploading: &Mutex<HashSet<String>>) -> anyhow::Result<Value> {
    let mut tasks = Vec::new();
    let busy = uploading
        .lock()
        .map_err(|_| anyhow::anyhow!("上传状态不可用"))?
        .clone();
    for task in session::tasks(root)? {
        let changed = session::local_path(root, &task)
            .and_then(|p| session::digest(&p))
            .map(|h| h != task.baseline_digest);
        let pending = journal::pending(root, &task.task_id);
        let automatic = auto_upload::settings(root, &task.task_id);
        let lease = journal::lease(root, &task.task_id);
        let acknowledged = journal::lease_acknowledged(root, &task.task_id);
        let closed = journal::closed(root, &task.task_id);
        let action = pending.as_ref().ok().and_then(|value| {
            value.as_ref().map(|i| {
                match i.action {
                    journal::Action::OptimisticUpdate => "optimistic-update",
                    journal::Action::Commit => "commit",
                    journal::Action::Checkin => "checkin",
                }
                .to_string()
            })
        });
        tasks.push(json!({"task_id":task.task_id,"file_name":task.file_name,"mode":task.mode,
            "local_changed":changed.as_ref().copied().unwrap_or(false),"needs_review":changed.is_err() || pending.is_err() || automatic.is_err(),
            "can_upload":task.mode == "optimistic-edit" && task.device_id.is_some(),
            "exclusive":task.mode == "exclusive-edit" && task.device_id.is_some(),
            "has_lease":lease.as_ref().map(|v| v.is_some()).unwrap_or(false),
            "lease_acknowledged":acknowledged.unwrap_or(false),
            "checkout_closed":closed.unwrap_or(false),
            "auto_upload":automatic.ok(),"uploading":busy.contains(&task.task_id),
            "pending_upload":pending.ok().flatten().is_some(),"pending_action":action}));
    }
    Ok(json!({"ok":true,"cdc_enabled":cloudfile_core::cdc::enabled(),"tasks":tasks}))
}
#[tauri::command]
async fn save_cloud(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let result = upload::save(&root, &task_id).map_err(|e| e.to_string())?;
        if result["state"] == "published" {
            let _ = auto_upload::confirmed(&root, &task_id);
        }
        Ok(result)
    })
    .await
    .map_err(|_| "上传任务执行失败".to_string())?
}
#[tauri::command]
async fn recover_upload(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let result = upload::recover(&root, &task_id).map_err(|e| e.to_string())?;
        if result["state"] == "published" {
            let _ = auto_upload::confirmed(&root, &task_id);
        }
        Ok(result)
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
            .map_err(|e| e.to_string())
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
        .map(|origin| json!({"state":device::pairing_state(&state.root,&origin),"service_url":format!("{}{}",origin,session::service_path(&state.root,&origin).unwrap_or_default()),"origin":origin}))
        .collect::<Vec<_>>()))
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
fn set_auto_upload(
    state: tauri::State<'_, State>,
    task_id: String,
    enabled: bool,
) -> Result<(), String> {
    auto_upload::set_enabled(&state.root, &task_id, enabled).map_err(|e| e.to_string())
}
#[tauri::command]
fn trust_origin(state: tauri::State<'_, State>, origin: String) -> Result<(), String> {
    session::trust(&state.root, origin)
        .map_err(|_| "请输入完整服务地址，例如 https://files.example.com".into())
}
#[tauri::command]
fn trusted_origins(state: tauri::State<'_, State>) -> Result<Vec<String>, String> {
    session::config(&state.root)
        .map(|x| x.allowed_origins)
        .map_err(|_| "配置读取失败".into())
}
#[tauri::command]
fn open_task(state: tauri::State<'_, State>, task_id: String) -> Result<(), String> {
    session::launch(&state.root, &task_id).map_err(|_| "无法打开工作副本".into())
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
        exclusive::commit(&root, &task_id, checkin).map_err(|e| e.to_string())
    })
    .await
    .map_err(|_| "提交任务执行失败".to_string())?
}
/// Recovers the original commit intent; never creates a new one.
#[tauri::command]
async fn recover_commit(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::recover(&root, &task_id).map_err(|e| e.to_string())
    })
    .await
    .map_err(|_| "查询提交执行失败".to_string())?
}
#[tauri::command]
async fn renew_checkout(state: tauri::State<'_, State>, task_id: String) -> Result<Value, String> {
    let root = state.root.clone();
    tauri::async_runtime::spawn_blocking(move || {
        exclusive::renew_checkout(&root, &task_id).map_err(|e| e.to_string())
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
        exclusive::release(&root, &task_id).map_err(|e| e.to_string())
    })
    .await
    .map_err(|_| "释放 Checkout 执行失败".to_string())?
}
fn main() {
    if let Err(e) = run() {
        eprintln!("CloudFile desktop: {e}");
    }
}
fn run() -> anyhow::Result<()> {
    let root = storage::root()?;
    let _lock = match storage::lock(&root.join("desktop.lock")) {
        Ok(l) => l,
        Err(_) => return Ok(()),
    };
    ipc::recover(&root)?;
    let uploading = Arc::new(Mutex::new(HashSet::new()));
    tauri::Builder::default().manage(State{root:root.clone(),uploading:uploading.clone()})
 .invoke_handler(tauri::generate_handler![task_summary,trust_origin,trusted_origins,open_task,save_cloud,recover_upload,begin_pairing,service_status,set_auto_upload,commit_task,recover_commit,renew_checkout,release_checkout])
 .setup(move|app|{
  use tauri::{menu::{Menu,MenuItem},tray::TrayIconBuilder};
  let open=MenuItem::with_id(app,"open","打开任务中心",true,None::<&str>)?;
  let quit=MenuItem::with_id(app,"quit","退出",true,None::<&str>)?;
  let menu=Menu::with_items(app,&[&open,&quit])?;
  let icon=tauri::image::Image::new_owned([30u8,118,238,255].repeat(32*32),32,32);
  // Window remains visible as fallback if the OS cannot create a tray icon.
  let tray=TrayIconBuilder::new().icon(icon).tooltip("CloudFile Agent 2.2 Dev").menu(&menu).on_menu_event(|app,event|match event.id.as_ref(){"open"=>show(app),"quit"=>app.exit(0),_=>()}).build(app);
  let has_tray=tray.is_ok();
  let window=app.get_webview_window("main").unwrap();let handle=app.handle().clone();
  window.on_window_event(move|e|{if let tauri::WindowEvent::CloseRequested{api,..}=e{if has_tray{api.prevent_close();if let Some(w)=handle.get_webview_window("main"){let _=w.hide();}}}});
  let handle=app.handle().clone();
  let auto_root=root.clone();let auto_handle=handle.clone();let auto_busy=uploading.clone();
  std::thread::spawn(move||{
   let started=std::time::Instant::now();let mut observer=auto_upload::Observer::default();
   loop{
    let mut changed=false;
    let result=observer.scan_with(&auto_root,started.elapsed().as_millis() as u64,|id,digest|{
     changed=true;
     if let Ok(mut busy)=auto_busy.lock(){busy.insert(id.to_string());}
     use tauri::Emitter;let _=auto_handle.emit("tasks-changed",());
     let result=upload::save_observed(&auto_root,id,digest);
     if let Ok(mut busy)=auto_busy.lock(){busy.remove(id);}
     result
    });
    if result.is_err(){eprintln!("CloudFile automatic upload storage requires review");}
    if changed{use tauri::Emitter;let _=auto_handle.emit("tasks-changed",());}
    std::thread::sleep(std::time::Duration::from_secs(2));
   }
  });
  std::thread::spawn(move||loop{
   let result=ipc::drain(&root,|request|match request{
    wire::Request::Status {}=>wire::status(),
    wire::Request::PairEnrollment{server}=>match device::enrollment(&root,&server){Ok(value)=>json!({"ok":true,"enrollment":value,"service_path":session::service_path(&root,&server).unwrap_or_default()}),Err(e)=>wire::error(&e.to_string())},
    wire::Request::PairSign{server,response}=>match device::pair_sign(&root,&server,response){Ok(value)=>json!({"ok":true,"proof":value}),Err(e)=>wire::error(&e.to_string())},
    wire::Request::PairConfirm{server,response}=>match device::confirm(&root,&server,response){Ok(())=>{show(&handle);json!({"ok":true})},Err(e)=>wire::error(&e.to_string())},
    wire::Request::OpenUri{uri}=>match paired_session::open(&root,&uri){Ok(task)=>{show(&handle);match session::launch(&root,&task.task_id){Ok(())=>json!({"ok":true}),Err(_)=>wire::error("文件已保留，未能打开本地应用，请在任务中心查看")}},Err(e)=>wire::error(&e.to_string())},
    wire::Request::Tasks {}=>summaries(&root,&uploading).unwrap_or_else(|_|wire::error("task storage unavailable")),
    wire::Request::Focus{task_id}=>{if session::load(&root,&task_id).is_ok(){show(&handle);use tauri::Emitter;let _=handle.emit("focus-task",task_id);json!({"ok":true})}else{wire::error("task unavailable")}},
    r@wire::Request::Open{..}=>match session::open_session(&root,r){Ok(task)=>{show(&handle);match session::launch(&root,&task.task_id){Ok(())=>json!({"ok":true}),Err(_)=>wire::error("download retained; local application could not open file")}},Err(_)=>wire::error("session rejected or download failed; inspect trusted origin and server session")},
   });if result.is_err(){eprintln!("CloudFile mailbox operation failed");}
   std::thread::sleep(std::time::Duration::from_millis(150));
  });Ok(())
 }).run(tauri::generate_context!())?;
    Ok(())
}
