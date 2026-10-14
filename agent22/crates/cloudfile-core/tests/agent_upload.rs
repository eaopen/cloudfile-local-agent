use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use cloudfile_core::{
    device::{self, Challenge, Client},
    journal, paired_session, session, upload,
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
#[derive(Default)]
struct ServerState {
    uploads: Vec<Vec<u8>>,
    intent: Value,
    baseline: String,
    challenges: Vec<Value>,
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
        self.join.take().unwrap().join().unwrap();
    }
}
fn request(stream: &mut TcpStream) -> (String, std::collections::HashMap<String, String>, Vec<u8>) {
    stream
        .set_read_timeout(Some(Duration::from_secs(5)))
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
impl Server {
    fn start(key: &SigningKey, scenario: &'static str) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = origin.clone();
        let state = Arc::new(Mutex::new(ServerState {
            baseline: "a".repeat(40),
            ..Default::default()
        }));
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
                let route = if scenario == "prefix" {
                    route
                        .strip_prefix("/seafile")
                        .expect("all requests use the configured service path")
                        .to_string()
                } else {
                    route
                };
                assert!(!headers.contains_key("cookie"));
                assert!(!headers.contains_key("transfer-encoding"));
                let mut state = s.lock().unwrap();
                if route == "/seafhttp/cloudfile/read" {
                    assert_eq!(headers["authorization"], format!("Bearer {SID}"));
                    reply(&mut stream, 200, b"initial");
                    continue;
                }
                let operation = route.trim_end_matches('/').rsplit('/').next().unwrap();
                if operation == "optimistic-upload" {
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
                    assert_eq!(
                        format!("{:x}", Sha256::digest(&data)),
                        state.intent["content_digest"]
                    );
                    state.uploads.push(data);
                    if scenario == "conflict" {
                        reply(&mut stream, 409, b"{}");
                        continue;
                    }
                    state.baseline = if state.uploads.len() == 1 {
                        "b".repeat(40)
                    } else {
                        "c".repeat(40)
                    };
                    state.intent["state"] = json!("published");
                    state.intent["result_file_id"] = json!(state.baseline);
                    if scenario == "lost" && state.uploads.len() == 1 {
                        continue;
                    }
                    let mut receipt = json!({"intent_id":state.intent["intent_id"],"state":"published","base_file_id":state.intent["base_file_id"],"result_file_id":state.baseline});
                    if scenario == "wrong-receipt" {
                        receipt["intent_id"] = json!(uuid::Uuid::new_v4());
                    }
                    reply(&mut stream, 200, &serde_json::to_vec(&receipt).unwrap());
                    continue;
                }
                assert!(!headers.contains_key("authorization"));
                let body: Value = serde_json::from_slice(&data).unwrap();
                assert_eq!(body["device_id"], DID);
                assert_eq!(body["session_id"], SID);
                let result = if operation == "challenge" || operation.ends_with("challenge") {
                    let op = if operation == "challenge" {
                        "claim"
                    } else if operation == "read-challenge" {
                        "read"
                    } else {
                        "commit"
                    };
                    let nonce = B64.encode(Sha256::digest(format!("{}", state.challenges.len())));
                    let c = json!({"instance":server,"device_id":DID,"session_id":SID,"operation":op,"nonce":nonce,"issued_at":cloudfile_core::now(),"expires_at":cloudfile_core::now()+60,"request_sha256":"a".repeat(64)});
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
                            json!({"ticket":SID,"expires_in":60,"transfer":{"path":"/seafhttp/cloudfile/read","method":"GET","authorization":"Bearer","redirects":false,"resume":false},"file":{"extension":".txt","mode":"optimistic-edit","base_version":state.baseline}})
                        }
                        "optimistic-upload-authorize" => {
                            assert_eq!(body["base_file_id"], state.baseline);
                            state.intent = json!({"intent_id":body["intent_id"],"state":"prepared","base_file_id":body["base_file_id"],"content_digest":body["content_digest"],"snapshot_size":body["snapshot_size"],"result_file_id":""});
                            json!({"ticket":format!("v1.{}","A".repeat(60)),"expires_in":60,"intent_id":body["intent_id"],"content_digest":body["content_digest"],"snapshot_size":body["snapshot_size"],"native_device_publish":true,"transfer":{"path":if scenario=="bad-grant"{"https://evil.example/upload"}else{"/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/"},"method":"POST","authorization":"Bearer","redirects":false}})
                        }
                        "optimistic-status" => json!({"intent":state.intent}),
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
    let key = SigningKey::from_slice(&[1u8; 32]).unwrap();
    let server = Server::start(&key, scenario);
    let client = Client::with_key(&server.origin, DID, SID, key)
        .unwrap()
        .with_service_path(if scenario == "prefix" { "/seafile" } else { "" })
        .unwrap();
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
fn local_edit_upload_continue_and_empty_file() {
    let (_tmp, root, server, client, task) = setup("ok");
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"edited in local application").unwrap();
    assert_eq!(
        upload::save_with(&root, &task.task_id, &client).unwrap()["state"],
        "published"
    );
    assert_eq!(
        session::load(&root, &task.task_id)
            .unwrap()
            .baseline_file_id,
        "b".repeat(40)
    );
    assert_eq!(
        upload::save_with(&root, &task.task_id, &client).unwrap()["state"],
        "unchanged"
    );
    std::fs::write(&path, b"").unwrap();
    assert_eq!(
        upload::save_with(&root, &task.task_id, &client).unwrap()["state"],
        "published"
    );
    assert_eq!(
        session::load(&root, &task.task_id)
            .unwrap()
            .baseline_file_id,
        "c".repeat(40)
    );
    assert_eq!(
        server.state.lock().unwrap().uploads,
        vec![b"edited in local application".to_vec(), vec![]]
    );
}
#[test]
fn lost_ack_queries_same_intent_without_uploading_new_edits() {
    let (_tmp, root, server, client, task) = setup("lost");
    let path = session::local_path(&root, &task).unwrap();
    std::fs::write(&path, b"first change").unwrap();
    assert!(upload::save_with(&root, &task.task_id, &client).is_err());
    let intent = journal::pending(&root, &task.task_id).unwrap().unwrap();
    std::fs::write(&path, b"later local edits").unwrap();
    assert_eq!(
        upload::save_with(&root, &task.task_id, &client).unwrap()["state"],
        "published"
    );
    assert!(journal::pending(&root, &task.task_id).unwrap().is_none());
    assert_eq!(server.state.lock().unwrap().uploads.len(), 1);
    assert_eq!(
        session::load(&root, &task.task_id).unwrap().baseline_digest,
        intent.content_digest
    );
    assert_eq!(std::fs::read(path).unwrap(), b"later local edits");
}
#[test]
fn conflict_wrong_receipt_and_bad_destination_keep_local_edits() {
    for scenario in ["conflict", "wrong-receipt", "bad-grant"] {
        let (_tmp, root, server, client, task) = setup(scenario);
        let path = session::local_path(&root, &task).unwrap();
        std::fs::write(&path, b"important local edits").unwrap();
        assert!(upload::save_with(&root, &task.task_id, &client).is_err());
        assert_eq!(std::fs::read(path).unwrap(), b"important local edits");
        assert!(journal::pending(&root, &task.task_id).unwrap().is_some());
        assert_eq!(
            session::load(&root, &task.task_id)
                .unwrap()
                .baseline_file_id,
            "a".repeat(40)
        );
        if scenario == "bad-grant" {
            assert!(server.state.lock().unwrap().uploads.is_empty());
        }
    }
}
#[test]
fn challenge_matches_existing_go_hub_fixture_and_rejects_other_operation() {
    let c = Challenge {
        instance: "https://etech.example".into(),
        device_id: DID.into(),
        session_id: SID.into(),
        operation: "commit".into(),
        nonce: B64.encode((0u8..32).collect::<Vec<_>>()),
        issued_at: 1791734999,
        expires_at: 1791735059,
        request_sha256: "a".repeat(64),
    };
    assert_eq!(
        format!("{:x}", Sha256::digest(c.message().unwrap())),
        "151b7a256dcd805d360846444510f2be2a12862de7c092bd093ca62f63d1e4e2"
    );
    let current = Challenge {
        issued_at: cloudfile_core::now(),
        expires_at: cloudfile_core::now() + 60,
        ..c
    };
    assert!(current
        .validate(&current.instance, DID, SID, "commit")
        .is_ok());
    assert!(current
        .validate(&current.instance, DID, SID, "read")
        .is_err());
    assert!(current
        .validate("https://other.example", DID, SID, "commit")
        .is_err());
    assert!(!device::encoded("bad", 32));
}

#[test]
fn deployment_prefix_keeps_origin_bound_proof_and_raw_upload() {
    let (_tmp, root, server, client, task) = setup("prefix");
    let local = session::local_path(&root, &task).unwrap();
    std::fs::write(local, b"prefixed dev edit").unwrap();
    assert_eq!(
        upload::save_with(&root, &task.task_id, &client).unwrap()["state"],
        "published"
    );
    assert_eq!(
        server.state.lock().unwrap().uploads,
        vec![b"prefixed dev edit".to_vec()]
    );
    assert_eq!(client.server, server.origin);
}
