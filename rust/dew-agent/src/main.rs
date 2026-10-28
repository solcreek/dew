use std::time::Duration;

/// Bounds how much unsynced guest data an abrupt VM stop can lose; the host
/// attaches the disk Cached+Fsync, so a guest sync(2) becomes a host fsync.
const SYNC_INTERVAL: Duration = Duration::from_secs(10);

#[cfg(target_os = "linux")]
fn main() {
    use dew_agent::{env::DEFAULT_PATH, server, vsock};
    use std::sync::Arc;

    // Pin PATH first: argv[0] is resolved against the agent's own PATH, so
    // its order (util-linux before BusyBox) decides which binary runs.
    std::env::set_var("PATH", DEFAULT_PATH);

    let agent = Arc::new(server::Agent::new(std::env::var("DEW_EXEC_USER").ok()));

    // Sleep between syncs rather than on a fixed tick, so a slow sync under
    // heavy write never degrades into back-to-back syncs.
    std::thread::spawn(|| loop {
        std::thread::sleep(SYNC_INTERVAL);
        unsafe { libc::sync() };
    });

    let port = match std::env::var("DEW_VSOCK_PORT") {
        Ok(p) => p.parse::<u32>().unwrap_or_else(|e| {
            eprintln!("invalid DEW_VSOCK_PORT: {e}");
            std::process::exit(1)
        }),
        Err(_) => dew_proto::DEFAULT_PORT,
    };
    let listener = vsock::listen(port).unwrap_or_else(|e| {
        eprintln!("vsock listen on port {port}: {e}");
        std::process::exit(1)
    });
    eprintln!("dew-agent: listening on vsock port {port}");

    loop {
        match vsock::accept(&listener) {
            Ok(conn) => {
                let agent = agent.clone();
                std::thread::spawn(move || server::handle_conn(conn, &agent));
            }
            Err(e) => eprintln!("accept: {e}"),
        }
    }
}

#[cfg(not(target_os = "linux"))]
fn main() {
    let _ = SYNC_INTERVAL;
    eprintln!("dew-agent runs inside the Linux guest only");
    std::process::exit(1);
}
