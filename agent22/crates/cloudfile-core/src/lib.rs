pub mod ipc;
pub mod journal;
pub mod session;
pub mod storage;
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
