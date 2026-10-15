use crate::{session, storage};
use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use p256::ecdsa::{signature::Signer, Signature, SigningKey};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    io::Read,
    path::{Path, PathBuf},
    time::Duration,
};
pub const API: &str = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/";
pub const MAX_FILE: u64 = 128 * 1024 * 1024;
pub fn hex(value: &str, len: usize) -> bool {
    value.len() == len
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
pub fn uuid(value: &str) -> bool {
    uuid::Uuid::parse_str(value).is_ok_and(|u| u.to_string() == value)
}
pub fn encoded(value: &str, len: usize) -> bool {
    B64.decode(value)
        .is_ok_and(|b| b.len() == len && B64.encode(b) == value)
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Challenge {
    pub instance: String,
    pub device_id: String,
    pub session_id: String,
    pub operation: String,
    pub nonce: String,
    pub issued_at: u64,
    pub expires_at: u64,
    pub request_sha256: String,
}
impl Challenge {
    pub fn validate(&self, server: &str, did: &str, sid: &str, op: &str) -> Result<()> {
        session::origin(&self.instance)?;
        if self.instance != server
            || self.device_id != did
            || self.session_id != sid
            || self.operation != op
            || !uuid(did)
            || !uuid(sid)
            || !encoded(&self.nonce, 32)
            || !hex(&self.request_sha256, 64)
            || self.expires_at <= self.issued_at
            || self.expires_at - self.issued_at > 120
            || self.expires_at > 9007199254740991
            || crate::now() < self.issued_at
            || crate::now() >= self.expires_at
        {
            bail!("设备挑战不匹配或已过期");
        }
        Ok(())
    }
    pub fn message(&self) -> Result<Vec<u8>> {
        let mut value = serde_json::to_value(self)?;
        value["v"] = json!(1);
        let mut data = b"cloudfile.device-proof.v1\n".to_vec();
        data.extend(serde_json::to_vec(&value)?);
        Ok(data)
    }
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Pairing {
    pub instance: String,
    pub key_alias: String,
    pub device_id: String,
    pub thumbprint: String,
    pub expires_at: u64,
}
fn path(root: &Path, server: &str, pending: bool) -> Result<PathBuf> {
    session::origin(server)?;
    Ok(root.join("pairings").join(format!(
        "{}{:x}.json",
        if pending { "pending-" } else { "" },
        Sha256::digest(server)
    )))
}
fn trusted(root: &Path, server: &str) -> Result<()> {
    session::origin(server)?;
    if !session::config(root)?
        .allowed_origins
        .contains(&server.to_string())
    {
        bail!("请先在任务中心添加受信任服务");
    }
    Ok(())
}
#[cfg(any(target_os = "macos", target_os = "windows"))]
fn entry(alias: &str) -> Result<keyring::Entry> {
    if !uuid(alias) {
        bail!("invalid key alias");
    }
    Ok(keyring::Entry::new("com.cloudfile.agent22.device", alias)?)
}
fn read_key(alias: &str) -> Result<SigningKey> {
    #[cfg(any(target_os = "macos", target_os = "windows"))]
    {
        let bytes = zeroize::Zeroizing::new(
            entry(alias)?
                .get_secret()
                .context("系统凭据库中的设备密钥不可用")?,
        );
        Ok(SigningKey::from_slice(&bytes)?)
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let _ = alias;
        bail!("设备密钥仅支持 Windows/macOS");
    }
}
fn public(key: &SigningKey) -> Value {
    let p = key.verifying_key().to_encoded_point(false);
    json!({"crv":"P-256","kty":"EC","x":B64.encode(p.x().unwrap()),"y":B64.encode(p.y().unwrap())})
}
fn thumb(key: &SigningKey) -> Result<String> {
    Ok(B64.encode(Sha256::digest(serde_json::to_vec(&public(key))?)))
}
/// Local desktop action only. Browsers may finish this short-lived enrollment, never initiate it.
pub fn begin(root: &Path, server: &str) -> Result<()> {
    trusted(root, server)?;
    let _lock = storage::lock(&root.join("pairing.lock"))?;
    if path(root, server, false)?.exists() {
        bail!("此服务已配对");
    }
    if let Ok(p) = pending(root, server) {
        read_key(&p.key_alias)?;
        return Ok(());
    }
    let key = SigningKey::random(&mut p256::elliptic_curve::rand_core::OsRng);
    let alias = uuid::Uuid::new_v4().to_string();
    #[cfg(any(target_os = "macos", target_os = "windows"))]
    entry(&alias)?
        .set_secret(&key.to_bytes())
        .context("无法保存到系统凭据库")?;
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    bail!("设备配对仅支持 Windows/macOS");
    storage::write(
        &path(root, server, true)?,
        &Pairing {
            instance: server.into(),
            key_alias: alias,
            device_id: String::new(),
            thumbprint: thumb(&key)?,
            expires_at: crate::now() + 600,
        },
    )
}
fn pending(root: &Path, server: &str) -> Result<Pairing> {
    trusted(root, server)?;
    let p: Pairing =
        storage::read(&path(root, server, true)?).context("请先在任务中心点击连接设备")?;
    if p.instance != server || p.expires_at <= crate::now() {
        bail!("配对已过期，请在任务中心重新连接");
    }
    Ok(p)
}
pub fn enrollment(root: &Path, server: &str) -> Result<Value> {
    let p = pending(root, server)?;
    let key = read_key(&p.key_alias)?;
    if thumb(&key)? != p.thumbprint {
        bail!("设备密钥不匹配");
    }
    Ok(json!({"public_key":public(&key)}))
}
pub fn pair_sign(root: &Path, server: &str, response: Value) -> Result<Value> {
    let _lock = storage::lock(&root.join("pairing.lock"))?;
    let mut p = pending(root, server)?;
    let key = read_key(&p.key_alias)?;
    let c: Challenge = serde_json::from_value(response["challenge"].clone())?;
    c.validate(server, &c.device_id, &c.session_id, "pair")?;
    let hash = format!(
        "{:x}",
        Sha256::digest(serde_json::to_vec(
            &json!({"device_id":c.device_id,"pairing_id":c.session_id,"key_thumbprint":p.thumbprint})
        )?)
    );
    if response["device_id"] != c.device_id
        || response["state"] != "pending"
        || response["revision"] != "1"
        || response["key_thumbprint"] != p.thumbprint
        || thumb(&key)? != p.thumbprint
        || c.request_sha256 != hash
    {
        bail!("配对响应不匹配");
    }
    let signature: Signature = key.sign(&c.message()?);
    p.device_id = c.device_id.clone();
    storage::write(&path(root, server, true)?, &p)?;
    Ok(
        json!({"device_id":p.device_id,"nonce":c.nonce,"signature":B64.encode(signature.to_bytes())}),
    )
}
pub fn confirm(root: &Path, server: &str, response: Value) -> Result<()> {
    let _lock = storage::lock(&root.join("pairing.lock"))?;
    let mut p = pending(root, server)?;
    if !uuid(&p.device_id)
        || response["device_id"] != p.device_id
        || response["state"] != "active"
        || response["revision"] != "2"
        || thumb(&read_key(&p.key_alias)?)? != p.thumbprint
    {
        bail!("设备配对未确认");
    }
    if path(root, server, false)?.exists() {
        bail!("已有设备配对，不能替换");
    }
    p.expires_at = 0;
    storage::write(&path(root, server, false)?, &p)?;
    std::fs::remove_file(path(root, server, true)?)?;
    Ok(())
}
pub fn paired(root: &Path, server: &str) -> Result<Pairing> {
    trusted(root, server)?;
    let p: Pairing = storage::read(&path(root, server, false)?).context("请先完成设备配对")?;
    if p.instance != server || p.expires_at != 0 || !uuid(&p.device_id) {
        bail!("设备配对无效");
    }
    Ok(p)
}
pub fn pairing_state(root: &Path, server: &str) -> &'static str {
    if paired(root, server).is_ok() {
        "paired"
    } else if pending(root, server).is_ok() {
        "pending"
    } else {
        "unpaired"
    }
}
pub struct Client {
    service_path: String,
    pub http: reqwest::blocking::Client,
    pub server: String,
    pub device_id: String,
    pub session_id: String,
    key: SigningKey,
}
impl Client {
    pub fn new(root: &Path, server: &str, sid: &str) -> Result<Self> {
        let p = paired(root, server)?;
        let key = read_key(&p.key_alias)?;
        if thumb(&key)? != p.thumbprint {
            bail!("设备密钥已变更");
        }
        Self::with_key(server, &p.device_id, sid, key)?
            .with_service_path(&session::service_path(root, server)?)
    }
    /// Injectable in-memory key for protocol tests; production calls `new` with an OS-protected pairing.
    pub fn with_key(server: &str, did: &str, sid: &str, key: SigningKey) -> Result<Self> {
        session::origin(server)?;
        if !uuid(did) || !uuid(sid) {
            bail!("invalid device session");
        }
        Ok(Self {
            http: reqwest::blocking::Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .timeout(Duration::from_secs(300))
                .build()?,
            server: server.into(),
            service_path: String::new(),
            device_id: did.into(),
            session_id: sid.into(),
            key,
        })
    }
    pub fn with_service_path(mut self, prefix: &str) -> Result<Self> {
        if !session::valid_service_path(prefix) {
            bail!("invalid service path");
        }
        self.service_path = prefix.into();
        Ok(self)
    }
    pub fn endpoint(&self, path: &str) -> String {
        format!("{}{}{path}", self.server, self.service_path)
    }
    pub fn body(&self) -> Value {
        json!({"session_id":self.session_id,"device_id":self.device_id})
    }
    pub fn post(&self, route: &str, body: &Value) -> Result<Value> {
        if ![
            "challenge",
            "claim",
            "read-challenge",
            "read-ticket",
            // Exclusive Checkout handoff: a current paired device may only
            // receive, acknowledge or renew its own guarded reservation.
            "lease-challenge",
            "lease-receive",
            "lease-confirm-challenge",
            "lease-confirm",
            "lease-renew-challenge",
            "lease-renew",
            "lease-abandon-challenge",
            "lease-abandon",
            // Signed single-file CommitIntent and its receipt.
            "commit-challenge",
            "commit-authorize",
            "commit-upload-authorize",
            "commit-status-challenge",
            "commit-status",
            // Optimistic local edit publication.
            "optimistic-upload-challenge",
            "optimistic-upload-authorize",
            "optimistic-status-challenge",
            "optimistic-status",
        ]
        .contains(&route)
        {
            bail!("unsupported device operation");
        }
        let res = self
            .http
            .post(self.endpoint(&format!("{API}{route}/")))
            .timeout(Duration::from_secs(30))
            .json(body)
            .send()
            .map_err(|_| anyhow::anyhow!("服务暂时不可达；本地修改已保留"))?;
        decode(res)
    }
    pub fn signed(&self, route: &str, operation: &str, body: &mut Value) -> Result<()> {
        let value = self.post(route, body)?;
        let c: Challenge = serde_json::from_value(value["challenge"].clone())?;
        c.validate(&self.server, &self.device_id, &self.session_id, operation)?;
        let sig: Signature = self.key.sign(&c.message()?);
        body["signature"] = json!(B64.encode(sig.to_bytes()));
        body["challenge"] = serde_json::to_value(c)?;
        Ok(())
    }
}
pub fn decode(response: reqwest::blocking::Response) -> Result<Value> {
    let status = response.status();
    let mut data = Vec::new();
    response
        .take(65537)
        .read_to_end(&mut data)
        .context("响应中断；请查询原上传结果")?;
    if data.len() > 65536 {
        bail!("服务响应过大");
    }
    if status != 200 {
        match status.as_u16() {
            409 => bail!("云端文件或会话已变化；本地修改已保留，请勿覆盖云端"),
            401 | 403 => bail!("设备授权已失效或没有权限；本地修改已保留"),
            503 => bail!("服务暂不可用或尚未启用 Agent 上传；本地修改已保留"),
            _ => bail!("服务未确认操作（HTTP {}）；本地修改已保留", status.as_u16()),
        }
    }
    Ok(serde_json::from_slice(&data)?)
}

#[cfg(all(test, any(target_os = "macos", target_os = "windows")))]
mod tests {
    use super::*;
    use p256::ecdsa::signature::Verifier;
    #[test]
    #[ignore = "creates and removes a temporary credential in the current user's OS store"]
    fn pairing_roundtrip_in_os_store() {
        struct Cleanup(String);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = entry(&self.0).unwrap().delete_credential();
            }
        }
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().canonicalize().unwrap();
        let server = "https://agent22-pairing-test.invalid";
        session::trust(&root, server.into()).unwrap();
        assert!(enrollment(&root, server).is_err());
        begin(&root, server).unwrap();
        let pending_pair = pending(&root, server).unwrap();
        let _cleanup = Cleanup(pending_pair.key_alias.clone());
        let public_value = enrollment(&root, server).unwrap();
        assert_eq!(public_value["public_key"]["kty"], "EC");
        let did = uuid::Uuid::new_v4().to_string();
        let sid = uuid::Uuid::new_v4().to_string();
        let hash=format!("{:x}",Sha256::digest(serde_json::to_vec(&json!({"device_id":did,"pairing_id":sid,"key_thumbprint":pending_pair.thumbprint})).unwrap()));
        let challenge = Challenge {
            instance: server.into(),
            device_id: did.clone(),
            session_id: sid,
            operation: "pair".into(),
            nonce: B64.encode([1u8; 32]),
            issued_at: crate::now(),
            expires_at: crate::now() + 60,
            request_sha256: hash,
        };
        let response = json!({"device_id":did,"state":"pending","revision":"1","key_thumbprint":pending_pair.thumbprint,"challenge":challenge});
        let proof = pair_sign(&root, server, response).unwrap();
        let sig = Signature::from_slice(&B64.decode(proof["signature"].as_str().unwrap()).unwrap())
            .unwrap();
        read_key(&pending_pair.key_alias)
            .unwrap()
            .verifying_key()
            .verify(&challenge.message().unwrap(), &sig)
            .unwrap();
        assert!(confirm(
            &root,
            server,
            json!({"device_id":uuid::Uuid::new_v4(),"state":"active","revision":"2"})
        )
        .is_err());
        // The actual Hub pair-confirm response deliberately has no key_thumbprint.
        confirm(
            &root,
            server,
            json!({"device_id":did,"state":"active","revision":"2"}),
        )
        .unwrap();
        assert_eq!(paired(&root, server).unwrap().device_id, did);
        assert!(enrollment(&root, server).is_err());
        assert!(begin(&root, server).is_err());
    }
}

/// Read-only readiness probe before creating a local pairing credential.
pub fn preflight(root: &Path, server: &str) -> Result<()> {
    trusted(root, server)?;
    let prefix = session::service_path(root, server)?;
    let http = reqwest::blocking::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(Duration::from_secs(10))
        .build()?;
    let response = http
        .get(format!("{server}{prefix}/api/v2.1/cloudfile/capabilities/"))
        .send()
        .map_err(|_| anyhow::anyhow!("无法连接服务，请检查地址和网络"))?;
    let value = decode(response).map_err(|_| {
        anyhow::anyhow!("服务未返回 CloudFile 能力信息，请填写完整服务地址（含部署路径）")
    })?;
    if value["capabilities"]["local.open-edit"]["enabled"] != true {
        bail!("此服务尚未启用 Agent 设备编辑，请先完成服务端配置；未创建配对密钥");
    }
    let response = http
        .get(format!(
            "{server}{prefix}/api/v2.1/cloudfile/extensions/local-edit/v1/devices/pair-start/"
        ))
        .send()
        .map_err(|_| anyhow::anyhow!("设备接口不可达"))?;
    if response.status() != 405 {
        bail!("服务尚未开放设备配对接口；未创建配对密钥");
    }
    Ok(())
}
