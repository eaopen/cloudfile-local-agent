use cloudfile_core::{ipc, storage, wire};
fn main() {
    if run().is_err() {
        eprintln!("CloudFile native messaging failed");
        std::process::exit(1);
    }
}
fn run() -> anyhow::Result<()> {
    let root = storage::root()?;
    let mut input = std::io::stdin().lock();
    let mut output = std::io::stdout().lock();
    while let Some(value) = wire::read(&mut input)? {
        let response = match serde_json::from_value::<wire::Request>(value) {
            Ok(wire::Request::Status {}) => wire::status(),
            Ok(request) => ipc::call(&root, request).unwrap_or_else(|_| {
                wire::error("desktop unavailable or response unknown; inspect task center")
            }),
            Err(_) => wire::error("invalid or unsupported native message"),
        };
        wire::write(&mut output, &response)?;
    }
    Ok(())
}
