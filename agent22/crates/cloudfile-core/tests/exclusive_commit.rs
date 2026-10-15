//! Exclusive Checkout handoff and manual Commit/Checkin against a device-only
//! mock Hub. Nothing here falls back to browser authorization or ordinary
//! Seafile upload APIs; a rejected or lost publication must keep local bytes.
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use cloudfile_core::{
    device::{Challenge, Client},
    exclusive, journal, paired_session, session,
};
use p256::ecdsa::{signature::Verifier, Signature, SigningKey};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    thread,
    time::Duration,
};
const DID: &str = "11111111-1111-4111-8111-111111111111";
const SID: &str = "22222222-2222-4222-8222-222222222222";
const GUARD: &str = "33333333-3333-4333-8333-333333333333";
const BASE: &str = "a";
const PUBLISHED: &str = "b";
#[derive(Default)]
struct ServerState {
    uploads: Vec<Vec<u8>>,
    intent: Value,
    challenges: Vec<Value>,
    confirmations: u32,
    released: bool,
}
fn lease_value() -> Value {
    json!({"guard_id":GUARD,"generation":"1","credential_epoch":"1",
        "base_file_id":BASE.repeat(40),"token":"f".repeat(64)})
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
        // A mock server that served a surprising request must fail the test
        // normally. Panicking again here would turn it into an abort that
        // masks the real assertion and wedges the whole binary.
        if let Some(join) = self.join.take() {
            let _ = join.join();
        }
    }
}
fn request(stream: &mut TcpStream) -> (String, std::collections::HashMap<String, String>, Vec<u8>) {
    // The listening socket is non-blocking, and CI runners are loaded: be
    // explicit about blocking mode and allow a generous read budget.
    stream.set_nonblocking(false).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(30)))
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
fn published_receipt(state: &ServerState, digest: &str, action: &str) -> Value {
    json!({"intent_id":state.intent["intent_id"],"state":"published","result_file_id":PUBLISHED.repeat(40),
        "content_digest":digest,"expected_file_id":BASE.repeat(40),"action":action,
        "checked_in":action == "checkin"})
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
                // A device proof never carries a browser cookie or a caller
                // supplied identity; only the signed body is accepted.
                assert!(!headers.contains_key("cookie"));
                assert!(!headers.contains_key("transfer-encoding"));
                let mut state = s.lock().unwrap();
                if route == "/seafhttp/cloudfile/read" {
                    assert_eq!(headers["authorization"], format!("Bearer {SID}"));
                    reply(&mut stream, 200, b"initial");
                    continue;
                }
                let operation = route.trim_end_matches('/').rsplit('/').next().unwrap();
                if operation == "commit-upload" {
                    assert_eq!(headers["content-type"], "application/octet-stream");
                    assert!(headers["authorization"].starts_with("Bearer v1."));
                    assert_eq!(
                        headers["idempotency-key"],
                        state.intent["intent_id"].as_str().unwrap()
                    );
                    assert_eq!(
                        data.len() as u64,
                        state.intent["snapshot_size"].as_u64().unwrap()
                    );
                    let digest = format!("{:x}", Sha256::digest(&data));
                    assert_eq!(digest, state.intent["content_digest"]);
                    state.uploads.push(data.clone());
                    if scenario == "conflict" {
                        reply(&mut stream, 409, b"{}");
                        continue;
                    }
                    let action = state.intent["action"].as_str().unwrap().to_string();
                    let receipt = json!({"intent":published_receipt(&state, &digest, &action)});
                    if scenario == "lost" && state.uploads.len() == 1 {
                        // The native publication happened; the reply was lost.
                        continue;
                    }
                    let bytes = if scenario == "wrong-receipt" {
                        let mut wrong = receipt.clone();
                        wrong["intent"]["intent_id"] = json!(uuid::Uuid::new_v4());
                        serde_json::to_vec(&wrong).unwrap()
                    } else {
                        serde_json::to_vec(&receipt).unwrap()
                    };
                    reply(&mut stream, 200, &bytes);
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
                        "commit-status-challenge" | "commit-challenge" => "commit",
                        _ => "lease",
                    };
                    let nonce = B64.encode(Sha256::digest(state.challenges.len().to_string()));
                    let c = json!({"instance":server,"device_id":DID,"session_id":SID,"operation":op,
                        "nonce":nonce,"issued_at":cloudfile_core::now(),"expires_at":cloudfile_core::now()+60,
                        "request_sha256":"a".repeat(64)});
                    state.challenges.push(c.clone());
                    json!({"challenge":c})
                } else {
                    let c: Challenge = serde_json::from_value(body["challenge"].clone()).unwrap();
                    assert!(state.challenges.contains(&body["challenge"]));
                    state.challenges.retain(|v| v != &body["challenge"]);
                    let sig = Signature::from_slice(
                        &B64.decode(body["signature"].as_str().unwrap()).unwrap(),
                    )
                    .unwrap();
                    verify.verify(&c.message().unwrap(), &sig).unwrap();
                    match operation {
                        "claim" => {
                            assert!(body["ticket"].is_string());
                            json!({"session_id":SID,"state":"claimed"})
                        }
                        "read-ticket" => {
                            assert!(body.get("ticket").is_none());
                            json!({"ticket":SID,"expires_in":60,"transfer":{"path":"/seafhttp/cloudfile/read","method":"GET","authorization":"Bearer","redirects":false,"resume":false},"file":{"extension":".txt","mode":"exclusive-edit","base_version":BASE.repeat(40)}})
                        }
                        "lease-receive" => json!({"session_id":SID,"lease":lease_value()}),
                        "lease-confirm" => {
                            state.confirmations += 1;
                            if state.confirmations > 1 {
                                // The server erases the ciphertext; a replay fails.
                                json!({"error":"conflict"})
                            } else {
                                json!({"confirmed":true,"session_id":SID})
                            }
                        }
                        "lease-renew" => json!({"session_id":SID,"renewed":true}),
                        "lease-abandon" => {
                            assert_eq!(body["guard_id"], GUARD);
                            assert_eq!(body["generation"], "1");
                            assert_eq!(body["token"], "f".repeat(64));
                            state.released = true;
                            json!({"session_id":SID,"state":"abandoned","released":true})
                        }
                        "commit-authorize" => {
                            assert_eq!(body["guard_id"], GUARD);
                            assert_eq!(body["base_file_id"], BASE.repeat(40));
                            assert!(matches!(
                                body["action"].as_str(),
                                Some("commit") | Some("checkin")
                            ));
                            state.intent = json!({"intent_id":body["intent_id"],"state":"prepared",
                                "content_digest":body["content_digest"],"snapshot_size":body["snapshot_size"],
                                "action":body["action"],"result_file_id":"","checked_in":false});
                            json!({"intent_id":body["intent_id"],"state":"prepared","device_authorized":true,"publication":"not_started"})
                        }
                        "commit-upload-authorize" => {
                            json!({"ticket":format!("v1.{}","A".repeat(60)),"expires_in":60,
                            "intent_id":body["intent_id"],"content_digest":body["content_digest"],
                            "snapshot_size":body["snapshot_size"],"native_device_publish":true,
                            "transfer":{"path":if scenario == "bad-grant" {"https://evil.example/upload"} else {"/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/"},"method":"POST","authorization":"Bearer","redirects":false}})
                        }
                        "commit-status" => {
                            let found = if state.intent.is_null()
                                || state.intent["intent_id"] != body["intent_id"]
                            {
                                json!({"intent_id":body["intent_id"],"state":"not-found"})
                            } else if state.uploads.is_empty() && scenario != "status-prepared" {
                                let mut prepared = state.intent.clone();
                                prepared["state"] = json!("prepared");
                                prepared
                            } else {
                                let digest = state.intent["content_digest"].as_str().unwrap();
                                let action = state.intent["action"].as_str().unwrap();
                                published_receipt(&state, digest, action)
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
fn setup(
    scenario: &'static str,
) -> (
    tempfile::TempDir,
    std::path::PathBuf,
    Server,
    Client,
    session::Task,
) {
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path().canonicalize().unwrap();
    let key = SigningKey::from_slice(&[2u8; 32]).unwrap();
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
    (tmp, root, server, client, task)
}
#[test]
fn exclusive_open_receives_and_acknowledges_a_guarded_checkout() {
    let (_tmp, root, server, client, task) = setup("ok");
    assert_eq!(task.mode, "exclusive-edit");
    let lease = journal::lease(&root, &task.task_id).unwrap().unwrap();
    assert!(exclusive::valid(&lease));
    assert_eq!(lease.token, "f".repeat(64));
    assert!(journal::lease_acknowledged(&root, &task.task_id).unwrap());
    assert_eq!(server.state.lock().unwrap().confirmations, 1);
    // A second open reuses the durable lease and never re-confirms it.
    paired_session::open_with(
        &root,
        &paired_session::Open {
            instance: server.origin.clone(),
            session_id: SID.into(),
            ticket: B64.encode([0u8; 32]),
        },
        &client,
    )
    .unwrap();
    assert_eq!(server.state.lock().unwrap().confirmations, 1);
    assert_eq!(
        std::fs::read(session::local_path(&root, &task).unwrap()).unwrap(),
        b"initial"
    );
}
#[test]
fn commit_publishes_the_frozen_snapshot_and_advances_the_baseline() {
    let (_tmp, root, server, client, task) = setup("ok");
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"exclusive local edit").unwrap();
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "published"
    );
    assert_eq!(
        server.state.lock().unwrap().uploads,
        vec![b"exclusive local edit".to_vec()]
    );
    let updated = session::load(&root, &task.task_id).unwrap();
    assert_eq!(updated.baseline_file_id, PUBLISHED.repeat(40));
    assert_eq!(
        updated.baseline_digest,
        format!("{:x}", Sha256::digest(b"exclusive local edit"))
    );
    assert!(!journal::closed(&root, &task.task_id).unwrap());
    // The retained Checkout follows the new baseline for the next commit.
    assert_eq!(
        journal::lease(&root, &task.task_id)
            .unwrap()
            .unwrap()
            .base_file_id,
        PUBLISHED.repeat(40)
    );
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "unchanged"
    );
}
#[test]
fn checkin_releases_the_checkout_and_closes_the_local_session() {
    let (_tmp, root, server, client, task) = setup("ok");
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"final content").unwrap();
    let result = exclusive::commit_with(&root, &task.task_id, &client, true).unwrap();
    assert_eq!(result["state"], "published");
    assert_eq!(result["checked_in"], true);
    assert!(journal::closed(&root, &task.task_id).unwrap());
    // The Checkout proof must not survive a completed checkin.
    assert!(journal::lease(&root, &task.task_id).unwrap().is_none());
    assert!(exclusive::commit_with(&root, &task.task_id, &client, false).is_err());
    assert!(exclusive::release_with(&root, &task.task_id, &client).is_err());
    assert_eq!(std::fs::read(&path).unwrap(), b"final content");
    assert_eq!(
        server.state.lock().unwrap().uploads,
        vec![b"final content".to_vec()]
    );
}
#[test]
fn lost_publication_reply_is_recovered_by_querying_the_original_intent() {
    let (_tmp, root, server, client, task) = setup("lost");
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"first exclusive change").unwrap();
    assert!(exclusive::commit_with(&root, &task.task_id, &client, false).is_err());
    let intent = journal::pending(&root, &task.task_id).unwrap().unwrap();
    assert_eq!(
        journal::pending(&root, &task.task_id)
            .unwrap()
            .unwrap()
            .intent_id,
        intent.intent_id
    );
    // A newer local edit must never replace the frozen original snapshot.
    std::fs::write(&path, b"later local edits").unwrap();
    assert_eq!(
        exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
        "pending"
    );
    assert_eq!(
        exclusive::recover_with(&root, &task.task_id, &client).unwrap()["state"],
        "published"
    );
    assert!(journal::pending(&root, &task.task_id).unwrap().is_none());
    assert_eq!(server.state.lock().unwrap().uploads.len(), 1);
    assert_eq!(std::fs::read(&path).unwrap(), b"later local edits");
    assert_eq!(
        session::load(&root, &task.task_id).unwrap().baseline_digest,
        format!("{:x}", Sha256::digest(b"first exclusive change"))
    );
}
#[test]
fn conflict_wrong_receipt_and_bad_destination_keep_local_bytes_and_intent() {
    for scenario in ["conflict", "wrong-receipt", "bad-grant"] {
        let (_tmp, root, server, client, task) = setup(scenario);
        let path = session::local_path(&root, &task).unwrap();
        std::fs::write(&path, b"important exclusive edits").unwrap();
        assert!(exclusive::commit_with(&root, &task.task_id, &client, false).is_err());
        assert_eq!(std::fs::read(&path).unwrap(), b"important exclusive edits");
        assert!(journal::pending(&root, &task.task_id).unwrap().is_some());
        assert_eq!(
            session::load(&root, &task.task_id)
                .unwrap()
                .baseline_file_id,
            BASE.repeat(40)
        );
        assert!(!journal::closed(&root, &task.task_id).unwrap());
        if scenario == "bad-grant" {
            assert!(server.state.lock().unwrap().uploads.is_empty());
        }
        // A pending intent blocks both a new commit and a Checkout release.
        assert_eq!(
            exclusive::commit_with(&root, &task.task_id, &client, false).unwrap()["state"],
            "pending"
        );
        assert!(exclusive::release_with(&root, &task.task_id, &client).is_err());
    }
}
#[test]
fn renewal_and_release_require_an_active_guarded_checkout() {
    let (_tmp, root, server, client, task) = setup("ok");
    assert_eq!(
        exclusive::renew_with(&root, &task.task_id, &client).unwrap()["state"],
        "renewed"
    );
    assert_eq!(
        exclusive::release_with(&root, &task.task_id, &client).unwrap()["state"],
        "released"
    );
    assert!(server.state.lock().unwrap().released);
    assert!(journal::closed(&root, &task.task_id).unwrap());
    assert!(exclusive::renew_with(&root, &task.task_id, &client).is_err());
}
#[test]
fn commit_rejects_unprepared_or_unleased_sessions() {
    let (_tmp, root, _server, client, task) = setup("ok");
    // A commit against a session whose journal holds no lease must fail closed.
    journal::close(&root, &task.task_id).unwrap();
    assert!(exclusive::commit_with(&root, &task.task_id, &client, false).is_err());
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"unsubmitted").unwrap();
    assert_eq!(std::fs::read(&path).unwrap(), b"unsubmitted");
}
