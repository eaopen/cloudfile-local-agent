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

pub const VERSION: &str = env!("CARGO_PKG_VERSION");
pub const HOST: &str = "com.cloudfile.agent22.dev";
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
pub mod paired_session;
pub mod upload;
