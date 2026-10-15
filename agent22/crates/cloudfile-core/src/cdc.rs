//! Bounded, device-authorized Seafile CDC block publication.
//!
//! A signed plan/stage/publish chain replaces the single full-file body only
//! while the already prepared CommitIntent, the frozen snapshot and a native
//! missing-block plan all agree. Staging a raw block never publishes a file:
//! the native guarded transaction behind `publish` stays the only write to the
//! file path and revalidates actor, device, lease, baseline and receipt there.
//! This module never takes a path from a browser and never falls back to an
//! ordinary Seafile upload API.
use crate::{
    device::{self, Client},
    exclusive::{self, Lease},
    journal::{Action, Intent, Receipt},
    session, upload,
};
use anyhow::{bail, Result};
use serde::Deserialize;
use serde_json::{json, Value};
use sha1::Sha1;
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    path::Path,
    sync::OnceLock,
    time::Duration,
};

/// Seafile's native v1 chunker. These bounds are part of the wire contract:
/// the server only accepts blocks between 1 byte and 10 MiB.
const MIN_SIZE: usize = 6 << 20;
const AVG_SIZE: u32 = 8 << 20;
const MAX_SIZE: usize = 10 << 20;
const WINDOW_SIZE: usize = 48;
const RABIN_POLY: u64 = 0xbfe6b8a5bf378d83;
/// `cf_device_block_plan` persists at most 64 ordered blocks per intent.
const MAX_BLOCKS: usize = 64;
/// Below this the extra plan and per-block grants cost more than they save.
const MIN_CDC_BYTES: u64 = 16 << 20;
/// One block operation must not outlive a few server-side grant lifetimes.
const BLOCK_TIMEOUT: Duration = Duration::from_secs(180);

/// The block transport is opt-in on both sides, exactly like the reference
/// agent. Unset keeps the already proven full-file transfer in use.
pub fn enabled() -> bool {
    std::env::var("CLOUDFILE_AGENT2_CDC_ENABLED").is_ok_and(|value| value == "1")
}

struct Rabin {
    t: [u64; 256],
    u: [u64; 256],
    shift: u32,
}
fn poly_mod(mut hi: u64, mut lo: u64, divisor: u64) -> u64 {
    let k = 63 - divisor.leading_zeros();
    let d = divisor << (63 - k);
    if hi & (1u64 << 63) != 0 {
        hi ^= d;
    }
    for i in (0..63).rev() {
        if hi & (1u64 << i) != 0 {
            hi ^= d >> (63 - i);
            lo ^= d << (i + 1);
        }
    }
    for i in (k..64).rev() {
        if lo & (1u64 << i) != 0 {
            lo ^= d >> (63 - i);
        }
    }
    lo
}
/// Returns the high and low halves of the carry-less product.
fn poly_multiply(x: u64, y: u64) -> (u64, u64) {
    let mut hi = 0u64;
    let mut lo = if x & 1 != 0 { y } else { 0 };
    for i in 1..64 {
        if x & (1u64 << i) != 0 {
            hi ^= y >> (64 - i);
            lo ^= y << i;
        }
    }
    (hi, lo)
}
fn poly_multiply_mod(x: u64, y: u64) -> u64 {
    let (hi, lo) = poly_multiply(x, y);
    poly_mod(hi, lo, RABIN_POLY)
}
fn append(shift: u32, t: &[u64; 256], p: u64, b: u8) -> u64 {
    // `p` is always reduced below 2^63, so the table index stays in range.
    (p << 8 | b as u64) ^ t[(p >> shift) as usize]
}
fn rabin() -> &'static Rabin {
    static TABLES: OnceLock<Rabin> = OnceLock::new();
    TABLES.get_or_init(|| {
        let shift = 63 - RABIN_POLY.leading_zeros();
        let low = shift - 8;
        let t1 = poly_mod(0, 1u64 << shift, RABIN_POLY);
        let mut t = [0u64; 256];
        for (i, slot) in t.iter_mut().enumerate() {
            let i = i as u64;
            *slot = poly_multiply_mod(i, t1) | (i << shift);
        }
        let mut pow = 1u64;
        for _ in 1..WINDOW_SIZE {
            pow = append(low, &t, pow, 0);
        }
        let mut u = [0u64; 256];
        for (i, slot) in u.iter_mut().enumerate() {
            *slot = poly_multiply_mod(i as u64, pow);
        }
        Rabin { t, u, shift: low }
    })
}
impl Rabin {
    fn roll(&self, p: u32, out: u8, into: u8) -> u32 {
        append(self.shift, &self.t, (p as u64) ^ self.u[out as usize], into) as u32
    }
    fn fingerprint(&self, buf: &[u8]) -> u32 {
        let mut p = 0u32;
        for &b in buf {
            p = self.roll(p, 0, b);
        }
        p
    }
}
/// Mirrors the reference chunker: the first window is hashed directly and the
/// remaining bytes roll one byte at a time until the low bits match. A block is
/// always cut at `MAX_SIZE` even when no boundary was found.
#[allow(clippy::needless_range_loop)]
fn find_cut(buf: &[u8], r: &Rabin) -> usize {
    if buf.len() < MIN_SIZE {
        return buf.len();
    }
    let mut fp = 0u32;
    for i in MIN_SIZE - 1..buf.len() {
        fp = if i == MIN_SIZE - 1 {
            r.fingerprint(&buf[i + 1 - WINDOW_SIZE..=i])
        } else {
            r.roll(fp, buf[i - WINDOW_SIZE], buf[i])
        };
        if fp & (AVG_SIZE - 1) == 0x13 || i + 1 >= MAX_SIZE {
            return i + 1;
        }
    }
    buf.len()
}

#[derive(Clone, Debug)]
pub struct Block {
    pub id: String,
    pub offset: u64,
    pub size: u64,
}
#[derive(Clone, Debug)]
pub struct Manifest {
    pub file_id: String,
    pub sha256: String,
    pub size: u64,
    pub blocks: Vec<Block>,
}
/// Seafile v1 file object identity: SHA-1 over Jansson's canonical JSON, which
/// keeps one space after each separator. The byte spacing is deliberate.
fn file_object_id(ids: &[String], size: u64) -> String {
    let joined = ids
        .iter()
        .map(|id| format!("\"{id}\""))
        .collect::<Vec<_>>()
        .join(", ");
    let data =
        format!("{{\"block_ids\": [{joined}], \"size\": {size}, \"type\": 1, \"version\": 1}}");
    format!("{:x}", Sha1::digest(data.as_bytes()))
}
/// Chunks one already frozen image, so hashed bytes and staged bytes cannot
/// diverge. The caller owns the 128 MiB bound check.
pub fn scan_bytes(data: &[u8]) -> Result<Manifest> {
    if data.is_empty() {
        bail!("CDC 需要至少 1 字节的常规快照");
    }
    let r = rabin();
    let mut blocks = Vec::new();
    let mut offset = 0usize;
    while offset < data.len() {
        let cut = find_cut(&data[offset..], r);
        if cut < 1 || cut > data.len() - offset || cut > MAX_SIZE {
            bail!("CDC 边界无效");
        }
        blocks.push(Block {
            id: format!("{:x}", Sha1::digest(&data[offset..offset + cut])),
            offset: offset as u64,
            size: cut as u64,
        });
        offset += cut;
    }
    let ids = blocks.iter().map(|b| b.id.clone()).collect::<Vec<_>>();
    Ok(Manifest {
        file_id: file_object_id(&ids, data.len() as u64),
        sha256: format!("{:x}", Sha256::digest(data)),
        size: data.len() as u64,
        blocks,
    })
}
fn freeze(source: &Path) -> Result<Vec<u8>> {
    let before = std::fs::symlink_metadata(source)?;
    if !before.is_file() || before.len() > device::MAX_FILE {
        bail!("冻结快照不是受限的常规文件");
    }
    let data = std::fs::read(source)?;
    let after = std::fs::symlink_metadata(source)?;
    if after.len() != before.len()
        || after.modified()? != before.modified()?
        || data.len() as u64 != before.len()
    {
        bail!("冻结快照在读取期间发生变化");
    }
    Ok(data)
}
/// Reads and chunks one frozen snapshot. Never mutates the input.
pub fn scan(path: &Path) -> Result<Manifest> {
    scan_bytes(&freeze(path)?)
}
/// Local cost heuristic only: the server's version and Guard fencing remain
/// authoritative. Requiring a real saving avoids paying CDC cost for documents
/// that reuse nothing.
pub fn worthwhile(total: u64, missing: u64) -> bool {
    total >= MIN_CDC_BYTES
        && missing <= total
        && total - missing >= 4 << 20
        && missing * 4 <= total * 3
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Commit,
    Optimistic,
}
impl Mode {
    fn prefix(self) -> &'static str {
        match self {
            Mode::Commit => "commit",
            Mode::Optimistic => "optimistic",
        }
    }
}
fn action(value: &Action) -> &'static str {
    match value {
        Action::Commit => "commit",
        Action::Checkin => "checkin",
        Action::OptimisticUpdate => "optimistic-update",
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Transfer {
    path: String,
    method: String,
    authorization: String,
    redirects: bool,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Grant {
    ticket: String,
    expires_in: u64,
    intent_id: uuid::Uuid,
    content_digest: String,
    snapshot_size: u64,
    native_device_publish: bool,
    transfer: Transfer,
}

struct Transport<'a> {
    client: &'a Client,
    intent: &'a Intent,
    mode: Mode,
}
impl Transport<'_> {
    /// Every block operation needs its own single-use proof: the previous
    /// ticket was consumed, and a replayed nonce never carries authority.
    fn ticket(&self, lease: Option<&Lease>) -> Result<String> {
        let (challenge, route, expected) = match self.mode {
            Mode::Commit => (
                "commit-challenge",
                "commit-upload-authorize",
                exclusive::COMMIT_UPLOAD,
            ),
            Mode::Optimistic => (
                "optimistic-upload-challenge",
                "optimistic-upload-authorize",
                upload::UPLOAD,
            ),
        };
        let mut body = match self.mode {
            Mode::Commit => exclusive::request(
                self.client,
                lease.ok_or_else(|| anyhow::anyhow!("提交模式缺少设备租约"))?,
                self.intent,
            )?,
            Mode::Optimistic => json!({
                "session_id": self.client.session_id,
                "device_id": self.client.device_id,
                "intent_id": self.intent.intent_id,
                "base_file_id": self.intent.base_file_id,
                "content_digest": self.intent.content_digest,
                "snapshot_size": self.intent.snapshot_size,
            }),
        };
        self.client.signed(challenge, "commit", &mut body)?;
        let grant: Grant = serde_json::from_value(self.client.post(route, &body)?)?;
        if grant.intent_id != self.intent.intent_id
            || grant.content_digest != self.intent.content_digest
            || grant.snapshot_size != self.intent.snapshot_size
            || !grant.native_device_publish
            || !(1..=60).contains(&grant.expires_in)
            || grant.transfer.path != expected
            || grant.transfer.method != "POST"
            || grant.transfer.authorization != "Bearer"
            || grant.transfer.redirects
            || !grant.ticket.starts_with("v1.")
            || !(43..=14000).contains(&grant.ticket.len())
            || !grant.ticket[3..]
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        {
            bail!("分块上传授权与原文件不匹配");
        }
        Ok(grant.ticket)
    }
    fn post(
        &self,
        operation: &str,
        ticket: &str,
        payload: Vec<u8>,
        content_type: &str,
        block_id: Option<&str>,
    ) -> Result<Value> {
        let url = self.client.endpoint(&format!(
            "{}{}-block-{operation}/",
            device::API,
            self.mode.prefix()
        ));
        let mut request = self
            .client
            .http
            .post(url)
            .timeout(BLOCK_TIMEOUT)
            .header("Content-Type", content_type)
            .header("Accept", "application/json")
            .header("Idempotency-Key", self.intent.intent_id.to_string())
            .bearer_auth(ticket)
            .body(payload);
        if let Some(id) = block_id {
            request = request.header("X-CloudFile-Block-ID", id);
        }
        device::decode(
            request
                .send()
                .map_err(|_| anyhow::anyhow!("分块响应中断；本地文件已保留，请查询原提交结果"))?,
        )
    }
}

fn receipt(mode: Mode, value: &Value, intent: &Intent) -> Result<Receipt> {
    let file = value["result_file_id"].as_str().unwrap_or("");
    if value["intent_id"] != intent.intent_id.to_string() || value["state"] != "published" {
        bail!("提交结果尚未确认，请查询原提交结果");
    }
    match mode {
        Mode::Commit => {
            let expected = value["expected_file_id"].as_str().unwrap_or("");
            if value["content_digest"].as_str() != Some(intent.content_digest.as_str())
                || expected != intent.base_file_id
                || value["action"].as_str() != Some(action(&intent.action))
                || value["checked_in"] != (intent.action == Action::Checkin)
                || !device::hex(file, 40)
            {
                bail!("服务端提交回执与原意图不匹配");
            }
            Ok(Receipt {
                intent_id: intent.intent_id,
                state: "published".into(),
                action: intent.action.clone(),
                base_file_id: expected.into(),
                content_digest: intent.content_digest.clone(),
                result_file_id: file.into(),
                checked_in: intent.action == Action::Checkin,
            })
        }
        Mode::Optimistic => {
            if value["base_file_id"] != intent.base_file_id
                || value["content_digest"] != intent.content_digest
                || !device::hex(file, 40)
            {
                bail!("上传结果尚未确认，请查询原上传结果");
            }
            Ok(Receipt {
                intent_id: intent.intent_id,
                state: "published".into(),
                action: Action::OptimisticUpdate,
                base_file_id: intent.base_file_id.clone(),
                content_digest: intent.content_digest.clone(),
                result_file_id: file.into(),
                checked_in: false,
            })
        }
    }
}

/// Publishes through the native block chain, or reports that the caller must
/// use the proven full-file transfer. `Ok(None)` is only ever returned while
/// nothing has been published, so falling back can never hide a publication.
pub fn publish(
    root: &Path,
    id: &str,
    client: &Client,
    intent: &Intent,
    mode: Mode,
    lease: Option<&Lease>,
) -> Result<Option<Receipt>> {
    publish_with(root, id, client, intent, mode, lease, enabled())
}
/// Injectable-gate variant used by protocol tests.
pub fn publish_with(
    root: &Path,
    id: &str,
    client: &Client,
    intent: &Intent,
    mode: Mode,
    lease: Option<&Lease>,
    engaged: bool,
) -> Result<Option<Receipt>> {
    if !engaged || intent.snapshot_size < MIN_CDC_BYTES {
        return Ok(None);
    }
    let dir = session::task_dir(root, id)?;
    let source = dir.join(format!("{}.snapshot", intent.intent_id));
    if !std::fs::symlink_metadata(&source)?.is_file() {
        bail!("原提交快照不可用");
    }
    let data = freeze(&source)?;
    let manifest = scan_bytes(&data)?;
    if manifest.size != intent.snapshot_size || manifest.sha256 != intent.content_digest {
        bail!("冻结快照与提交意图不一致，已停止发送");
    }
    if manifest.blocks.len() > MAX_BLOCKS {
        return Ok(None);
    }
    let transport = Transport {
        client,
        intent,
        mode,
    };
    // Failing to obtain a plan grant published nothing, so the full transfer is
    // still safe. An opt-in server that refuses CDC must not block small edits.
    let Ok(ticket) = transport.ticket(lease) else {
        return Ok(None);
    };
    let plan = serde_json::to_vec(&json!({
        "file_id": manifest.file_id,
        "size": manifest.size,
        "sha256": manifest.sha256,
        "blocks": manifest
            .blocks
            .iter()
            .map(|b| json!({"id": b.id, "size": b.size}))
            .collect::<Vec<_>>(),
    }))?;
    let Ok(planned) = transport.post("plan", &ticket, plan, "application/json", None) else {
        return Ok(None);
    };
    if planned["file_id"] != manifest.file_id
        || planned["intent_id"] != intent.intent_id.to_string()
        || planned["snapshot_size"] != manifest.size
    {
        bail!("服务端未确认 CDC 计划");
    }
    let mut sizes = BTreeMap::new();
    for block in &manifest.blocks {
        sizes.insert(block.id.clone(), block.size);
    }
    let Some(list) = planned["missing"].as_array() else {
        bail!("原生缺失分块响应不可信");
    };
    let mut seen = BTreeSet::new();
    let mut missing_bytes = 0u64;
    for value in list {
        let block_id = value
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("原生缺失分块响应不可信"))?;
        let size = *sizes
            .get(block_id)
            .ok_or_else(|| anyhow::anyhow!("原生缺失分块响应不可信"))?;
        if !seen.insert(block_id.to_string()) {
            bail!("原生缺失分块响应不可信");
        }
        missing_bytes += size;
    }
    if !worthwhile(manifest.size, missing_bytes) {
        // A successful plan response is not a publication, so returning to the
        // full transfer here keeps the same prepared intent and remains safe.
        return Ok(None);
    }
    for block_id in &seen {
        let block = manifest
            .blocks
            .iter()
            .find(|b| &b.id == block_id)
            .ok_or_else(|| anyhow::anyhow!("计划分块不可用"))?;
        let ticket = transport.ticket(lease)?;
        let start = block.offset as usize;
        let end = start + block.size as usize;
        let staged = transport.post(
            "stage",
            &ticket,
            data[start..end].to_vec(),
            "application/octet-stream",
            Some(block_id),
        )?;
        if staged["staged"] != true
            || staged["block_id"] != block_id.as_str()
            || staged["intent_id"] != intent.intent_id.to_string()
        {
            bail!("服务端未确认分块暂存");
        }
    }
    let ticket = transport.ticket(lease)?;
    // Publication carries no new bytes: the native guarded transaction
    // revalidates every staged or reused block before writing the file path.
    let published = transport.post(
        "publish",
        &ticket,
        Vec::new(),
        "application/octet-stream",
        None,
    )?;
    Ok(Some(receipt(mode, &published["intent"], intent)?))
}

#[cfg(test)]
mod tests {
    use super::*;
    /// Regenerates the deterministic image the reference chunker was run on.
    fn vector_bytes(n: usize) -> Vec<u8> {
        let mut out = vec![0u8; n];
        let mut state = 0x0123456789abcdefu64;
        for chunk in out.chunks_mut(8) {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            for (slot, byte) in chunk.iter_mut().zip(state.to_le_bytes()) {
                *slot = byte;
            }
        }
        out
    }
    /// Cross-checks the ported Rabin chunker against the reference agent's own
    /// output for a 32 MiB image, including a block cut at the 10 MiB cap.
    #[test]
    fn matches_the_reference_chunker_vector() {
        let manifest = scan_bytes(&vector_bytes(32 << 20)).unwrap();
        assert_eq!(manifest.size, 33554432);
        assert_eq!(
            manifest.sha256,
            "a964beae05db7b4aa22ffaa4f3ca28c823cad679746d995ca7392e903fbabfd9"
        );
        assert_eq!(manifest.file_id, "72a1aa0bcea4f7e2260656760d57eadcedcc951d");
        let expected = [
            (9332497u64, "ba662a00dba363c3684a1e8c29d9738f9069b370"),
            (10485760, "189e5f9f183e0da3eafa5c4d6444b9af90be3d40"),
            (7275587, "a16440c92207495b04298efa8a731bc95b456718"),
            (6298386, "85631f0c03adad716532f398f5a3b0102e943f11"),
            (162202, "5b3904b5d1f68ae66166e4aab06a5d48c26b0380"),
        ];
        assert_eq!(manifest.blocks.len(), expected.len());
        for (block, (size, id)) in manifest.blocks.iter().zip(expected) {
            assert_eq!((block.size, block.id.as_str()), (size, id));
        }
        // Offsets must tile the image exactly, or the plan cannot be accepted.
        let mut offset = 0;
        for block in &manifest.blocks {
            assert_eq!(block.offset, offset);
            offset += block.size;
        }
        assert_eq!(offset, manifest.size);
    }
    /// A one-byte file still yields the exact block identity the server checks.
    #[test]
    fn hashes_a_single_block_identity() {
        let manifest = scan_bytes(b"x").unwrap();
        assert_eq!(manifest.blocks.len(), 1);
        assert_eq!(manifest.blocks[0].size, 1);
        assert_eq!(
            manifest.file_id,
            file_object_id(&["11f6ad8ec52a2984abaafd7c3b516503785c2072".to_string()], 1)
        );
    }
    #[test]
    fn only_engages_cdc_when_it_saves_real_bytes() {
        // Below the 16 MiB floor the round trips cost more than they save.
        assert!(!worthwhile(15 << 20, 0));
        // Saves less than 4 MiB.
        assert!(!worthwhile(32 << 20, 31 << 20));
        // Reuses less than a quarter of the file.
        assert!(!worthwhile(32 << 20, 25 << 20));
        assert!(!worthwhile(32 << 20, 32 << 20));
        // A quarter or more reused, with a real byte saving.
        assert!(worthwhile(32 << 20, 20 << 20));
        assert!(worthwhile(32 << 20, 10 << 20));
        assert!(worthwhile(32 << 20, 0));
    }
}
