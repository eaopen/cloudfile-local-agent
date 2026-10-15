//! Same-user durable mailbox IPC: no TCP listener, credentials in argv or browser paths.
//! Desktop is the only consumer and holds desktop.lock for its entire lifetime.
use crate::{storage, wire};
use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{
    fs,
    path::Path,
    time::{Duration, Instant},
};
#[derive(Serialize, Deserialize)]
pub struct Envelope {
    pub expires_at: u64,
    pub request: wire::Request,
}
pub fn call(root: &Path, request: wire::Request) -> Result<Value> {
    let dir = root.join("ipc");
    storage::private_dir(&dir)?;
    let id = uuid::Uuid::new_v4();
    let input = dir.join(format!("{id}.request"));
    let output = dir.join(format!("{id}.response"));
    storage::write(
        &input,
        &Envelope {
            expires_at: crate::now() + 330,
            request,
        },
    )?;
    let exe = std::env::current_exe()?.with_file_name(if cfg!(windows) {
        "cloudfile-desktop.exe"
    } else {
        "cloudfile-desktop"
    });
    if !exe.is_file() {
        let _ = fs::remove_file(input);
        bail!("desktop executable missing beside native host");
    }
    let mut command = std::process::Command::new(exe);
    command
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(0x08000000);
    }
    match command.spawn() {
        Ok(mut child) => {
            std::thread::spawn(move || {
                let _ = child.wait();
            });
        }
        Err(e) => {
            let _ = fs::remove_file(input);
            return Err(e.into());
        }
    }
    let deadline = Instant::now() + Duration::from_secs(330);
    while Instant::now() < deadline {
        if output.exists() {
            let response = storage::read(&output);
            let _ = fs::remove_file(output);
            return response;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    // Do not re-submit after timeout: claim may already have consumed the ticket.
    bail!("desktop response unknown; inspect task center")
}
pub fn recover(root: &Path) -> Result<()> {
    let dir = root.join("ipc");
    storage::private_dir(&dir)?;
    for e in fs::read_dir(&dir)? {
        let p = e?.path();
        if p.extension().and_then(|x| x.to_str()) == Some("processing") {
            storage::write(
                &p.with_extension("response"),
                &wire::error("interrupted; inspect task center before retry"),
            )?;
            fs::remove_file(p)?;
        }
    }
    Ok(())
}
pub fn drain(root: &Path, mut handle: impl FnMut(wire::Request) -> Value) -> Result<()> {
    let dir = root.join("ipc");
    for e in fs::read_dir(&dir)? {
        let e = e?;
        if !e.file_type()?.is_file() {
            continue;
        }
        let p = e.path();
        if p.extension().and_then(|x| x.to_str()) != Some("request") {
            continue;
        }
        if uuid::Uuid::parse_str(p.file_stem().and_then(|x| x.to_str()).unwrap_or("")).is_err() {
            continue;
        }
        let active = p.with_extension("processing");
        fs::rename(&p, &active)?;
        let response = match storage::read::<Envelope>(&active) {
            Ok(e) if e.expires_at > crate::now() => handle(e.request),
            _ => wire::error("invalid or expired desktop request"),
        };
        storage::write(&p.with_extension("response"), &response)?;
        fs::remove_file(active)?;
    }
    Ok(())
}
