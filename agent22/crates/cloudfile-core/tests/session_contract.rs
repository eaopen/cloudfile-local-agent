use cloudfile_core::{session, storage, wire};
use serde_json::json;
use std::{
    io::{Read, Write},
    net::TcpListener,
    thread,
};
fn fixture(cross_origin: bool, redirect: bool) -> (String, thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let server = format!("http://{}", listener.local_addr().unwrap());
    let origin = server.clone();
    let handle = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(std::time::Duration::from_secs(5)))
            .unwrap();
        let mut data = [0; 8192];
        let n = stream.read(&mut data).unwrap();
        assert!(String::from_utf8_lossy(&data[..n]).contains("/agent-sessions/claim/"));
        let body=json!({"session_id":"11111111-1111-4111-8111-111111111111","mode":"local-edit","expires_at":cloudfile_core::now()+60,"repo_id":"22222222-2222-4222-8222-222222222222","path":"/report.txt","file":{"name":"report.txt","content_url":if cross_origin{"http://127.0.0.1:1/stolen".into()}else{format!("{origin}/content")}},"file_id":"a".repeat(40),"size":3}).to_string();
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )
        .unwrap();
        drop(stream);
        if !cross_origin {
            let (mut stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(std::time::Duration::from_secs(5)))
                .unwrap();
            let _ = stream.read(&mut data).unwrap();
            if redirect {
                write!(stream,"HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/stolen\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
            } else {
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nabc"
                )
                .unwrap();
            }
        }
    });
    (server, handle)
}
fn request(server: String) -> wire::Request {
    wire::Request::Open {
        protocol: "cloudfile-local/v2".into(),
        server,
        ticket: "one-use-ticket".into(),
        expires_at: cloudfile_core::now() + 60,
        local_action: String::new(),
    }
}
#[test]
fn claim_download_and_preserve_user_bytes() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().canonicalize().unwrap();
    let (server, handle) = fixture(false, false);
    session::trust(&root, server.clone()).unwrap();
    let task = session::open_session(&root, request(server)).unwrap();
    handle.join().unwrap();
    let local = session::local_path(&root, &task).unwrap();
    assert_eq!(std::fs::read(&local).unwrap(), b"abc");
    std::fs::write(&local, b"user changes").unwrap();
    let loaded = session::load(&root, &task.task_id).unwrap();
    assert_ne!(session::digest(&local).unwrap(), loaded.baseline_digest);
    assert_eq!(session::tasks(&root).unwrap().len(), 1);
    assert_eq!(std::fs::read(local).unwrap(), b"user changes");
}
#[test]
fn cross_origin_and_redirect_never_download() {
    for (cross, redirect) in [(true, false), (false, true)] {
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().canonicalize().unwrap();
        let (server, handle) = fixture(cross, redirect);
        session::trust(&root, server.clone()).unwrap();
        assert!(session::open_session(&root, request(server)).is_err());
        handle.join().unwrap();
        assert!(session::tasks(&root).unwrap().is_empty());
    }
}
#[test]
fn untrusted_origin_never_claims() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().canonicalize().unwrap();
    assert!(session::open_session(&root, request("http://127.0.0.1:1".into())).is_err());
}
#[test]
fn singleton_lock_and_interrupted_mailbox() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().canonicalize().unwrap();
    let lock = storage::lock(&root.join("desktop.lock")).unwrap();
    assert!(storage::lock(&root.join("desktop.lock")).is_err());
    drop(lock);
    assert!(storage::lock(&root.join("desktop.lock")).is_ok());
    let path = root
        .join("ipc")
        .join(format!("{}.processing", uuid::Uuid::new_v4()));
    storage::write(&path, &json!({"ticket":"secret"})).unwrap();
    cloudfile_core::ipc::recover(&root).unwrap();
    assert!(!path.exists());
    let value: serde_json::Value = storage::read(&path.with_extension("response")).unwrap();
    assert_eq!(value["ok"], false);
    assert!(!value.to_string().contains("secret"));
}

#[test]
fn service_address_accepts_deployment_path_without_changing_trust_origin() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().canonicalize().unwrap();
    session::trust(&root, "https://files.example.com/seafile/".into()).unwrap();
    assert_eq!(
        session::config(&root).unwrap().allowed_origins,
        vec!["https://files.example.com"]
    );
    assert_eq!(
        session::service_path(&root, "https://files.example.com").unwrap(),
        "/seafile"
    );
    for raw in [
        "https://files.example.com/a/../b",
        "https://files.example.com/seafile?token=x",
        "https://files.example.com//seafile",
        "https://files.example.com/%2e%2e",
    ] {
        assert!(session::trust(&root, raw.into()).is_err());
    }
}
#[test]
fn disabled_edit_capability_is_reported_before_pairing() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().canonicalize().unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    session::trust(&root, format!("{origin}/seafile")).unwrap();
    let worker = std::thread::spawn(move || {
        use std::io::{Read, Write};
        let (mut stream, _) = listener.accept().unwrap();
        let mut data = [0; 4096];
        let n = stream.read(&mut data).unwrap();
        assert!(String::from_utf8_lossy(&data[..n])
            .contains("GET /seafile/api/v2.1/cloudfile/capabilities/"));
        let body = r#"{"capabilities":{"local.open-edit":{"enabled":false}}}"#;
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )
        .unwrap();
    });
    let error = cloudfile_core::device::preflight(&root, &origin)
        .unwrap_err()
        .to_string();
    assert!(error.contains("尚未启用"));
    assert!(!root.join("pairings").exists());
    worker.join().unwrap();
}
