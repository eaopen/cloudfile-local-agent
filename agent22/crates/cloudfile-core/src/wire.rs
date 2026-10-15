use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::io::{Read, Write};
pub const LIMIT: usize = 1024 * 1024;
#[derive(Clone, Serialize, Deserialize)]
#[serde(tag = "type", deny_unknown_fields)]
pub enum Request {
    #[serde(rename = "open_uri")]
    OpenUri { uri: String },
    #[serde(rename = "pair_enrollment")]
    PairEnrollment { server: String },
    #[serde(rename = "pair_sign")]
    PairSign { server: String, response: Value },
    #[serde(rename = "pair_confirm")]
    PairConfirm { server: String, response: Value },
    #[serde(rename = "status")]
    Status {},
    #[serde(rename = "task_summary")]
    Tasks {},
    #[serde(rename = "focus_task")]
    Focus { task_id: String },
    #[serde(rename = "open_session")]
    Open {
        protocol: String,
        server: String,
        ticket: String,
        expires_at: u64,
        #[serde(default)]
        local_action: String,
    },
    /// Lightweight version probe the extension repeats periodically.
    #[serde(rename = "check_update")]
    CheckUpdate {},
    /// Explicit user action; the upgrade runs in a detached process because
    /// Chrome terminates this one as soon as the response is written.
    #[serde(rename = "update")]
    Update {},
    #[serde(rename = "update_extension")]
    UpdateExtension {},
    /// Does a working copy exist locally for this repository file?
    #[serde(rename = "query_local_file")]
    QueryLocalFile {
        repo_id: String,
        path: String,
        #[serde(default)]
        mode: String,
    },
    #[serde(rename = "query_local_folder")]
    QueryLocalFolder {
        repo_id: String,
        #[serde(default)]
        mode: String,
    },
    /// Opens the directory that holds working copies, never a browser path.
    #[serde(rename = "open_workspace")]
    OpenWorkspace {
        #[serde(default)]
        repo_id: String,
        #[serde(default)]
        path: String,
        #[serde(default)]
        mode: String,
    },
    /// Compatibility entry for a downloaded `.cloudfile` handoff file.
    #[serde(rename = "open_session_file")]
    OpenSessionFile { path: String },
}
pub fn read<R: Read>(input: &mut R) -> Result<Option<Value>> {
    let mut header = [0; 4];
    let n = input.read(&mut header[..1])?;
    if n == 0 {
        return Ok(None);
    }
    input.read_exact(&mut header[1..])?;
    let length = u32::from_ne_bytes(header) as usize;
    if length == 0 || length > LIMIT {
        bail!("invalid native message length");
    }
    let mut bytes = vec![0; length];
    input.read_exact(&mut bytes)?;
    Ok(Some(serde_json::from_slice(&bytes)?))
}
pub fn write<W: Write>(output: &mut W, response: &Value) -> Result<()> {
    let bytes = serde_json::to_vec(response)?;
    if bytes.len() > LIMIT {
        bail!("response too large");
    }
    output.write_all(&(bytes.len() as u32).to_ne_bytes())?;
    output.write_all(&bytes)?;
    output.flush()?;
    Ok(())
}
/// Minimal status used only when the agent store cannot be read. It reports no
/// capability, so the extension falls back to its documented degraded path
/// instead of assuming a feature that could not be checked.
pub fn degraded_status(reason: &str) -> Value {
    json!({"ok":false,"error":reason,"version":"2.2.0","build_version":crate::VERSION,
        "agent_family":"agent2","platform":std::env::consts::OS,"capabilities":[],
        "release_channel":"development"})
}
pub fn error(code: &str) -> Value {
    json!({"ok":false,"error":code})
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn frames_and_truncation() {
        let mut bytes = Vec::new();
        write(&mut bytes, &json!({"type":"status"})).unwrap();
        assert_eq!(
            read(&mut bytes.as_slice()).unwrap(),
            Some(json!({"type":"status"}))
        );
        assert!(read(&mut &bytes[..bytes.len() - 1]).is_err());
        assert!(read(&mut &u32::MAX.to_ne_bytes()[..]).is_err());
        assert!(read(&mut &[][..]).unwrap().is_none());
    }
    #[test]
    fn reject_browser_paths() {
        assert!(
            serde_json::from_value::<Request>(json!({"type":"status","path":"/tmp/a"})).is_err()
        );
        assert!(
            serde_json::from_value::<Request>(json!({"type":"commit","working_copy":"a"})).is_err()
        );
    }
}
