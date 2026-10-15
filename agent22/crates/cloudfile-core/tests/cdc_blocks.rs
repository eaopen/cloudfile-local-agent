//! Device-authorized CDC block publication against a device-only mock Hub.
//!
//! The mock re-checks the plan the client sends against the reference
//! chunker's own output, stages only planned blocks, and refuses to publish
//! until every ordered block is addressable. A rejection must keep local bytes
//! and the original intent, and must never silently become a different publish.
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use cloudfile_core::{
    cdc,
    device::{Challenge, Client},
    exclusive, journal, paired_session, session,
};
use p256::ecdsa::{signature::Verifier, Signature, SigningKey};
use serde_json::{json, Value};
use sha1::Sha1;
use sha2::{Digest, Sha256};
use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex, Once,
    },
    thread,
    time::Duration,
};
const DID: &str = "11111111-1111-4111-8111-111111111111";
const SID: &str = "22222222-2222-4222-8222-222222222222";
const GUARD: &str = "33333333-3333-4333-8333-333333333333";
const BASE: &str = "a";
const PUBLISHED: &str = "b";
const SIZE: u64 = 33554432;
const FILE_ID: &str = "72a1aa0bcea4f7e2260656760d57eadcedcc951d";
const SHA256: &str = "a964beae05db7b4aa22ffaa4f3ca28c823cad679746d995ca7392e903fbabfd9";
/// The reference chunker's ordered blocks for this exact image.
const REF: [(u64, &str); 5] = [
    (9332497, "ba662a00dba363c3684a1e8c29d9738f9069b370"),
    (10485760, "189e5f9f183e0da3eafa5c4d6444b9af90be3d40"),
    (7275587, "a16440c92207495b04298efa8a731bc95b456718"),
    (6298386, "85631f0c03adad716532f398f5a3b0102e943f11"),
    (162202, "5b3904b5d1f68ae66166e4aab06a5d48c26b0380"),
];

/// The block transport is opt-in, so the protocol tests must enable it exactly
/// once for the whole binary instead of racing `set_var` across threads.
fn enable_cdc() {
    static GATE: Once = Once::new();
    GATE.call_once(|| std::env::set_var("CLOUDFILE_AGENT2_CDC_ENABLED", "1"));
}
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
/// The exact canonical plan the server persists and re-derives, so a spacing or
/// key-order regression fails here instead of on a live library.
fn canonical_plan() -> String {
    let blocks = REF
        .iter()
        .map(|(size, id)| format!("{{\"id\":\"{id}\",\"size\":{size}}}"))
        .collect::<Vec<_>>()
        .join(",");
    format!(
        "{{\"blocks\":[{blocks}],\"file_id\":\"{FILE_ID}\",\"sha256\":\"{SHA256}\",\"size\":{SIZE}}}"
    )
}

#[derive(Default)]
struct ServerState {
    intent: Value,
    plans: usize,
    staged: Vec<String>,
    publishes: usize,
    full_uploads: usize,
    published: bool,
    nonce: usize,
}
struct Server {
    origin: String,
    state: Arc<Mutex<ServerState>>,
    stop: Arc<AtomicBool>,
    join: Option<thread::JoinHandle<()>>,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        // Fail the test normally; panicking again while unwinding would abort
        // the whole binary and hide the real assertion.
        if let Some(join) = self.join.take() {
            let _ = join.join();
        }
    }
}
type Received = (String, std::collections::HashMap<String, String>, Vec<u8>);
fn request(stream: &mut TcpStream) -> Received {
    // The listener is non-blocking and CI runners are loaded: be explicit.
    stream.set_nonblocking(false).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(60)))
        .unwrap();
    let mut head = Vec::new();
    while !head.ends_with(b"\r\n\r\n") {
        let mut b = [0];
        stream.read_exact(&mut b).unwrap();
        head.push(b[0]);
        assert!(head.len() < 20000);
    }
    let text = String::from_utf8(head).unwrap();
    let route = text
        .lines()
        .next()
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .to_string();
    let headers = text
        .lines()
        .skip(1)
        .filter_map(|l| l.split_once(':'))
        .map(|(k, v)| (k.to_ascii_lowercase(), v.trim().to_string()))
        .collect::<std::collections::HashMap<_, _>>();
    let len = headers
        .get("content-length")
        .map(|v| v.parse().unwrap())
        .unwrap_or(0);
    let mut data = vec![0; len];
    stream.read_exact(&mut data).unwrap();
    (route, headers, data)
}
fn reply(stream: &mut TcpStream, status: u16, bytes: &[u8]) {
    write!(stream,"HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",bytes.len()).unwrap();
    stream.write_all(bytes).unwrap();
}
fn lease_value() -> Value {
    json!({"guard_id":GUARD,"generation":"1","credential_epoch":"1",
        "base_file_id":BASE.repeat(40),"token":"f".repeat(64)})
}
fn receipt(state: &ServerState, file_id: &str) -> Value {
    json!({"intent_id":state.intent["intent_id"],"state":"published","result_file_id":file_id,
        "content_digest":SHA256,"expected_file_id":BASE.repeat(40),"action":"commit",
        "checked_in":false})
}
impl Server {
    fn start(key: &SigningKey, scenario: &'static str) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = origin.clone();
        let state = Arc::new(Mutex::new(ServerState::default()));
        let s = state.clone();
        let stop = Arc::new(AtomicBool::new(false));
        let done = stop.clone();
        let verify = *key.verifying_key();
        let join = thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                let (mut stream, _) = match listener.accept() {
                    Ok(v) => v,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                        continue;
                    }
                    Err(e) => panic!("{e}"),
                };
                let (route, headers, data) = request(&mut stream);
                assert!(!headers.contains_key("cookie"));
                assert!(!headers.contains_key("transfer-encoding"));
                if route == "/seafhttp/cloudfile/read" {
                    assert_eq!(headers["authorization"], format!("Bearer {SID}"));
                    // The baseline is an older revision, so the local edit below
                    // is a genuinely different image.
                    let body = b"previous revision";
                    write!(
                        stream,
                        "HTTP/1.1 200 Result\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    )
                    .unwrap();
                    stream.write_all(body).unwrap();
                    continue;
                }
                let operation = route.trim_end_matches('/').rsplit('/').next().unwrap();
                let mut state = s.lock().unwrap();
                if operation == "commit-upload" {
                    assert_eq!(headers["content-type"], "application/octet-stream");
                    assert!(headers["authorization"].starts_with("Bearer v1."));
                    assert_eq!(
                        headers["idempotency-key"],
                        state.intent["intent_id"].as_str().unwrap()
                    );
                    assert_eq!(data.len() as u64, SIZE);
                    assert_eq!(format!("{:x}", Sha256::digest(&data)), SHA256);
                    state.full_uploads += 1;
                    state.published = true;
                    let body = json!({"intent":receipt(&state, &PUBLISHED.repeat(40))});
                    reply(&mut stream, 200, &serde_json::to_vec(&body).unwrap());
                    continue;
                }
                if let Some(block_operation) = operation.strip_prefix("commit-block-") {
                    // Every block operation needs its own freshly consumed ticket.
                    assert!(headers["authorization"].starts_with("Bearer v1."));
                    assert_eq!(
                        headers["idempotency-key"],
                        state.intent["intent_id"].as_str().unwrap()
                    );
                    let intent_id = state.intent["intent_id"].clone();
                    match block_operation {
                        "plan" => {
                            assert_eq!(headers["content-type"], "application/json");
                            assert_eq!(String::from_utf8(data).unwrap(), canonical_plan());
                            if scenario == "unavailable" {
                                reply(&mut stream, 503, b"{}");
                                continue;
                            }
                            state.plans += 1;
                            // The library already holds most blocks, so only the
                            // first ordered block has to travel.
                            let missing: Vec<&str> = if scenario == "not-worthwhile" {
                                REF.iter().map(|(_, id)| *id).collect()
                            } else {
                                REF.iter().map(|(_, id)| *id).take(1).collect()
                            };
                            let body = json!({"file_id":FILE_ID,"missing":missing,
                                "snapshot_size":SIZE,"intent_id":intent_id});
                            reply(&mut stream, 200, &serde_json::to_vec(&body).unwrap());
                        }
                        "stage" => {
                            assert_eq!(headers["content-type"], "application/octet-stream");
                            let block_id = headers["x-cloudfile-block-id"].clone();
                            let expected = REF
                                .iter()
                                .find(|(_, id)| *id == block_id)
                                .unwrap_or_else(|| panic!("unplanned block {block_id}"))
                                .0;
                            assert_eq!(data.len() as u64, expected);
                            assert_eq!(format!("{:x}", Sha1::digest(&data)), block_id);
                            if scenario == "stage-conflict" {
                                reply(&mut stream, 409, b"{}");
                                continue;
                            }
                            state.staged.push(block_id.clone());
                            let body =
                                json!({"staged":true,"block_id":block_id,"intent_id":intent_id});
                            reply(&mut stream, 200, &serde_json::to_vec(&body).unwrap());
                        }
                        "publish" => {
                            // Publication carries no new bytes at all.
                            assert!(matches!(
                                headers.get("content-length").map(String::as_str),
                                None | Some("0")
                            ));
                            assert!(!headers.contains_key("x-cloudfile-block-id"));
                            assert!(data.is_empty());
                            state.publishes += 1;
                            state.published = true;
                            let body = json!({"intent":receipt(&state, FILE_ID)});
                            reply(&mut stream, 200, &serde_json::to_vec(&body).unwrap());
                        }
                        other => panic!("unexpected CDC operation {other}"),
                    }
                    continue;
                }
                assert!(!headers.contains_key("authorization"));
                let body: Value = serde_json::from_slice(&data).unwrap();
                assert_eq!(body["device_id"], DID);
                assert_eq!(body["session_id"], SID);
                let result = if operation.ends_with("challenge") {
                    let op = match operation {
                        "challenge" => "claim",
                        "read-challenge" => "read",
                        "commit-challenge" | "commit-status-challenge" => "commit",
                        _ => "lease",
                    };
                    let nonce = B64.encode(Sha256::digest(state.nonce.to_string()));
                    state.nonce += 1;
                    let c = json!({"instance":server,"device_id":DID,"session_id":SID,"operation":op,
                        "nonce":nonce,"issued_at":cloudfile_core::now(),"expires_at":cloudfile_core::now()+60,
                        "request_sha256":"a".repeat(64)});
                    json!({"challenge":c})
                } else {
                    let c: Challenge = serde_json::from_value(body["challenge"].clone()).unwrap();
                    let sig = Signature::from_slice(
                        &B64.decode(body["signature"].as_str().unwrap()).unwrap(),
                    )
                    .unwrap();
                    verify.verify(&c.message().unwrap(), &sig).unwrap();
                    match operation {
                        "claim" => json!({"session_id":SID,"state":"claimed"}),
                        "read-ticket" => {
                            json!({"ticket":SID,"expires_in":60,"transfer":{"path":"/seafhttp/cloudfile/read","method":"GET","authorization":"Bearer","redirects":false,"resume":false},"file":{"extension":".bin","mode":"exclusive-edit","base_version":BASE.repeat(40)}})
                        }
                        "lease-receive" => json!({"session_id":SID,"lease":lease_value()}),
                        "lease-confirm" => json!({"confirmed":true,"session_id":SID}),
                        "commit-authorize" => {
                            assert_eq!(body["guard_id"], GUARD);
                            assert_eq!(body["base_file_id"], BASE.repeat(40));
                            state.intent = json!({"intent_id":body["intent_id"],"state":"prepared",
                                "content_digest":body["content_digest"],"snapshot_size":body["snapshot_size"],
                                "action":body["action"],"result_file_id":"","checked_in":false});
                            json!({"intent_id":body["intent_id"],"state":"prepared","device_authorized":true,"publication":"not_started"})
                        }
                        "commit-upload-authorize" => {
                            assert!(body["guard_id"].is_string());
                            json!({"ticket":format!("v1.{}","A".repeat(60)),"expires_in":60,
                            "intent_id":body["intent_id"],"content_digest":body["content_digest"],
                            "snapshot_size":body["snapshot_size"],"native_device_publish":true,
                            "transfer":{"path":"/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/","method":"POST","authorization":"Bearer","redirects":false}})
                        }
                        "commit-status" => {
                            let found = if state.published {
                                let file = if state.publishes > 0 {
                                    FILE_ID
                                } else {
                                    &PUBLISHED.repeat(40)
                                };
                                receipt(&state, file)
                            } else {
                                let mut prepared = state.intent.clone();
                                prepared["state"] = json!("prepared");
                                prepared
                            };
                            json!({"intent":found})
                        }
                        _ => panic!("unexpected route: {route}"),
                    }
                };
                reply(&mut stream, 200, &serde_json::to_vec(&result).unwrap());
            }
        });
        Self {
            origin,
            state,
            stop,
            join: Some(join),
        }
    }
}
/// Opens an exclusive session whose baseline is an older revision, then writes
/// the reference image as the local edit. Every later assertion can therefore
/// rely on the reference chunker's own block identities.
fn setup(
    scenario: &'static str,
) -> (
    tempfile::TempDir,
    std::path::PathBuf,
    Server,
    Client,
    session::Task,
) {
    enable_cdc();
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path().canonicalize().unwrap();
    let key = SigningKey::from_slice(&[3u8; 32]).unwrap();
    let server = Server::start(&key, scenario);
    let client = Client::with_key(&server.origin, DID, SID, key).unwrap();
    let task = paired_session::open_with(
        &root,
        &paired_session::Open {
            instance: server.origin.clone(),
            session_id: SID.into(),
            ticket: B64.encode([0u8; 32]),
        },
        &client,
    )
    .unwrap();
    std::fs::write(
        session::local_path(&root, &task).unwrap(),
        vector_bytes(SIZE as usize),
    )
    .unwrap();
    (tmp, root, server, client, task)
}

#[test]
fn plan_matches_the_frozen_image_and_stages_only_missing_blocks() {
    let (_tmp, root, server, client, task) = setup("cdc");
    assert!(cdc::enabled());
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "published"
    );
    let state = server.state.lock().unwrap();
    assert_eq!(state.plans, 1);
    assert_eq!(state.publishes, 1);
    // Only the first ordered block was reported missing, so only it is sent:
    // the remaining four are reused straight from the library.
    assert_eq!(state.staged, vec![REF[0].1.to_string()]);
    assert_eq!(state.full_uploads, 0);
    drop(state);
    let updated = session::load(&root, &task.task_id).unwrap();
    assert_eq!(updated.baseline_file_id, FILE_ID);
    assert_eq!(updated.baseline_digest, SHA256);
    // The retained Checkout follows the published baseline for the next commit.
    assert_eq!(
        journal::lease(&root, &task.task_id)
            .unwrap()
            .unwrap()
            .base_file_id,
        FILE_ID
    );
}

#[test]
fn a_plan_that_saves_nothing_falls_back_to_the_full_transfer() {
    let (_tmp, root, server, client, task) = setup("not-worthwhile");
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "published"
    );
    let state = server.state.lock().unwrap();
    assert_eq!(state.plans, 1);
    // Nothing was staged and nothing was published through the block chain, so
    // the switch back to the full transfer cannot have hidden a publication.
    assert!(state.staged.is_empty() && state.publishes == 0);
    assert_eq!(state.full_uploads, 1);
}

#[test]
fn an_unavailable_block_transport_falls_back_to_the_full_transfer() {
    let (_tmp, root, server, client, task) = setup("unavailable");
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "published"
    );
    let state = server.state.lock().unwrap();
    assert_eq!(state.plans, 0);
    assert_eq!(state.publishes, 0);
    assert_eq!(state.full_uploads, 1);
}

#[test]
fn a_refused_stage_keeps_local_bytes_and_the_original_intent() {
    let (_tmp, root, server, client, task) = setup("stage-conflict");
    let path = session::local_path(&root, &task).unwrap();
    let local = std::fs::read(&path).unwrap();

    assert!(exclusive::commit_with(&root, &task.task_id, &client, false).is_err());
    let state = server.state.lock().unwrap();
    // A refused stage must never silently become a different publication.
    assert!(state.staged.is_empty() && state.publishes == 0 && state.full_uploads == 0);
    drop(state);
    // The local edit and the frozen snapshot survive for a later recovery.
    assert_eq!(std::fs::read(&path).unwrap(), local);
    let intent = journal::pending(&root, &task.task_id)
        .unwrap()
        .expect("the original intent must be retained");
    assert!(std::fs::symlink_metadata(
        session::task_dir(&root, &task.task_id)
            .unwrap()
            .join(format!("{}.snapshot", intent.intent_id))
    )
    .unwrap()
    .is_file());
    // The baseline was not advanced by a refused publication.
    let task = session::load(&root, &task.task_id).unwrap();
    assert_eq!(task.baseline_file_id, BASE.repeat(40));
}
