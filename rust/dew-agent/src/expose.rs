//! Guest-side listeners for `dew up --expose-host` (mirrors
//! startExposeForwarders / acceptExposeConns in cmd/dew-agent/main.go).
//!
//! For each exposed host port the guest listens on 127.0.0.2:<port>
//! (host.lo.internal resolves there, so it can't collide with 127.0.0.1
//! services in the same VM) and hands each accepted connection to a handler,
//! which tunnels it to the host.

use crate::conn;
use dew_proto::{write_frame, ReverseDialRequest, ReverseDialResponse, TYPE_REVERSE_DIAL};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::File;
use std::io;
use std::net::{IpAddr, TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::Duration;

/// The guest loopback alias exposed host ports listen on.
pub const EXPOSE_IP: &str = "127.0.0.2";

/// Called on its own thread for each accepted connection, with the port.
pub type Handler = Arc<dyn Fn(TcpStream, u16) + Send + Sync>;

/// Opens a stream to the host's reverse-forward listener (in the guest: vsock
/// CID 2, port 1025).
pub type HostDialer = Arc<dyn Fn() -> io::Result<File> + Send + Sync>;

/// How long to wait for the host's ReverseDialResponse (the Go agent's
/// reverseDialRespTimeout): a stalled host listener must not pin the guest
/// connection forever.
pub const REVERSE_DIAL_TIMEOUT: Duration = Duration::from_secs(5);

/// Tunnel one accepted guest connection to the host (handleExposeConn): dial
/// the host, ask it to dial 127.0.0.1:<port> on macOS, and on OK proxy both
/// ways. The host checks the token and that the port was exposed. Any
/// failure, a refusal, or no answer within `timeout` closes the guest
/// connection.
pub fn reverse_dial(tcp: TcpStream, port: u16, token: &str, dial: &HostDialer, timeout: Duration) {
    let Ok(mut host) = dial() else {
        return;
    };
    let req = ReverseDialRequest {
        kind: TYPE_REVERSE_DIAL.into(),
        token: token.into(),
        port: port.into(),
    };
    if write_frame(&mut host, &req).is_err() {
        return;
    }
    // One deadline for the whole answer frame, however the host paces it.
    let ok = conn::read_frame_bytes_within(&mut host, timeout)
        .ok()
        .and_then(|b| serde_json::from_slice::<ReverseDialResponse>(&b).ok())
        .is_some_and(|r| r.ok);
    if ok {
        conn::proxy(host, tcp);
    }
}

struct Listener {
    stop: Arc<AtomicBool>,
    thread: JoinHandle<()>,
}

/// Stop accepting on `listeners` and release their ports; returns once all
/// are closed. Every stop flag is set before any join, so the accept loops'
/// bounded polls (STOP_POLL_MS) overlap: closing N listeners takes about one
/// interval, not N, which keeps a large set_exposes inside the host's reply
/// deadline. No wake-up connection is needed (one could fail, e.g. under fd
/// exhaustion, and leave the port bound after the map forgot it).
fn close_all(listeners: impl IntoIterator<Item = Listener>) {
    let listeners: Vec<Listener> = listeners.into_iter().collect();
    for l in &listeners {
        l.stop.store(true, Ordering::SeqCst);
    }
    for l in listeners {
        let _ = l.thread.join();
    }
}

/// The set of running expose listeners.
pub struct Exposes {
    ip: IpAddr,
    handler: Handler,
    listeners: Mutex<BTreeMap<u16, Listener>>,
}

/// Close every listener, so dropping the set releases its ports instead of
/// detaching accept threads that keep them bound.
impl Drop for Exposes {
    fn drop(&mut self) {
        let running =
            std::mem::take(&mut *self.listeners.lock().unwrap_or_else(|e| e.into_inner()));
        close_all(running.into_values());
    }
}

impl Exposes {
    pub fn new(ip: IpAddr, handler: Handler) -> Exposes {
        Exposes {
            ip,
            handler,
            listeners: Mutex::new(BTreeMap::new()),
        }
    }

    /// Make the running listeners exactly `ports` (the desired full set):
    /// close those no longer wanted, open the new ones, keep the rest
    /// untouched. Ports outside 1..=65535 are ignored; one that fails to
    /// bind is logged and skipped, as in the Go agent. A re-send with a
    /// smaller set reclaims the dropped ports.
    pub fn reconcile(&self, ports: &[i64]) {
        let desired: BTreeSet<u16> = ports
            .iter()
            .filter_map(|&p| u16::try_from(p).ok())
            .filter(|&p| p != 0)
            .collect();
        let mut running = self.listeners.lock().unwrap();
        let gone: Vec<u16> = running
            .keys()
            .copied()
            .filter(|p| !desired.contains(p))
            .collect();
        close_all(gone.iter().filter_map(|p| running.remove(p)));
        for port in desired {
            if running.contains_key(&port) {
                continue;
            }
            match self.listen(port) {
                Ok(l) => {
                    running.insert(port, l);
                }
                Err(e) => eprintln!("expose: listen {}:{port}: {e}", self.ip),
            }
        }
    }

    /// The ports currently listening, ascending.
    pub fn ports(&self) -> Vec<u16> {
        self.listeners.lock().unwrap().keys().copied().collect()
    }

    fn listen(&self, port: u16) -> io::Result<Listener> {
        let ln = TcpListener::bind((self.ip, port))?;
        // Nonblocking so a ready-then-vanished connection can't block accept()
        // after poll said readable; the loop polls with a bounded wait.
        ln.set_nonblocking(true)?;

        let stop = Arc::new(AtomicBool::new(false));
        let (flag, handler) = (stop.clone(), self.handler.clone());
        let thread = thread::spawn(move || accept_loop(ln, port, &flag, &handler));
        Ok(Listener { stop, thread })
    }
}

/// Longest the accept loop waits before re-checking `stop`, which bounds how
/// long closing a listener takes.
const STOP_POLL_MS: libc::c_int = 100;

/// Where the accept loop gets connections (a TcpListener; a fake in tests,
/// which is how the accept-error path is reachable deterministically).
trait Acceptor {
    /// Wait up to STOP_POLL_MS for a connection to be ready.
    fn ready(&self) -> bool;
    fn accept(&self) -> io::Result<TcpStream>;
}

impl Acceptor for TcpListener {
    fn ready(&self) -> bool {
        use std::os::fd::AsRawFd;
        let mut pfd = libc::pollfd {
            fd: self.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        unsafe { libc::poll(&mut pfd, 1, STOP_POLL_MS) > 0 }
    }
    fn accept(&self) -> io::Result<TcpStream> {
        TcpListener::accept(self).map(|(c, _)| c)
    }
}

fn accept_loop(ln: impl Acceptor, port: u16, stop: &AtomicBool, handler: &Handler) {
    while !stop.load(Ordering::SeqCst) {
        if !ln.ready() || stop.load(Ordering::SeqCst) {
            continue; // timeout or EINTR: re-check stop
        }
        match ln.accept() {
            Ok(conn) => {
                // Accepted sockets inherit O_NONBLOCK on BSD/macOS; the
                // handler does blocking I/O.
                if conn.set_nonblocking(false).is_err() {
                    continue;
                }
                let h = handler.clone();
                thread::spawn(move || h(conn, port));
            }
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
                ) => {}
            // Transient (e.g. EMFILE): keep the listener, don't spin. The Go
            // agent's loop returns here and leaves a dead entry in its map.
            Err(_) => thread::sleep(Duration::from_millis(50)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::Ipv4Addr;

    const LO: IpAddr = IpAddr::V4(Ipv4Addr::LOCALHOST);

    /// A port that was free a moment ago.
    fn free_port() -> u16 {
        TcpListener::bind((LO, 0))
            .unwrap()
            .local_addr()
            .unwrap()
            .port()
    }

    /// Handler that echoes one line back prefixed with its port.
    fn echo() -> Handler {
        Arc::new(|mut c: TcpStream, port: u16| {
            let mut b = [0u8; 64];
            let n = c.read(&mut b).unwrap_or(0);
            let _ = c.write_all(format!("{port}:{}", String::from_utf8_lossy(&b[..n])).as_bytes());
        })
    }

    fn ask(port: u16, msg: &str) -> io::Result<String> {
        let mut c = TcpStream::connect((LO, port))?;
        c.set_read_timeout(Some(Duration::from_secs(5)))?;
        c.write_all(msg.as_bytes())?;
        let mut out = String::new();
        c.read_to_string(&mut out)?;
        Ok(out)
    }

    #[test]
    fn listens_on_each_port_and_passes_it_to_the_handler() {
        let (a, b) = (free_port(), free_port());
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64, b as i64]);
        assert_eq!(ask(a, "hi").unwrap(), format!("{a}:hi"));
        assert_eq!(ask(b, "yo").unwrap(), format!("{b}:yo"));
        let mut want = vec![a, b];
        want.sort();
        assert_eq!(x.ports(), want);
    }

    #[test]
    fn a_smaller_set_closes_and_releases_dropped_ports() {
        let (a, b) = (free_port(), free_port());
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64, b as i64]);
        x.reconcile(&[a as i64]);
        assert_eq!(x.ports(), vec![a]);
        assert!(ask(b, "x").is_err(), "dropped port still accepting");
        // Released, not just ignored: it can be bound again right away.
        TcpListener::bind((LO, b)).expect("port reclaimed");
        assert_eq!(ask(a, "still").unwrap(), format!("{a}:still"));
    }

    #[test]
    fn an_empty_set_closes_everything() {
        let a = free_port();
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64]);
        x.reconcile(&[]);
        assert!(x.ports().is_empty());
        assert!(ask(a, "x").is_err());
    }

    #[test]
    fn re_adding_a_just_dropped_port_works() {
        let a = free_port();
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64]);
        x.reconcile(&[]);
        x.reconcile(&[a as i64]);
        assert_eq!(ask(a, "again").unwrap(), format!("{a}:again"));
    }

    /// The accept thread serving `port`: a restarted listener gets a new one.
    fn listener_thread(x: &Exposes, port: u16) -> thread::ThreadId {
        x.listeners.lock().unwrap()[&port].thread.thread().id()
    }

    #[test]
    fn unchanged_ports_are_left_running() {
        // A reconcile that keeps a port must not restart its listener. (An
        // already-accepted connection would survive a restart, so asserting
        // on one couldn't catch it; the accept thread's identity does.)
        let (a, b) = (free_port(), free_port());
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64]);
        let before = listener_thread(&x, a);
        x.reconcile(&[a as i64, a as i64, b as i64]);
        assert_eq!(
            listener_thread(&x, a),
            before,
            "listener for {a} was restarted"
        );
        x.reconcile(&[b as i64, a as i64]);
        assert_eq!(
            listener_thread(&x, a),
            before,
            "listener for {a} was restarted"
        );
        assert_eq!(ask(a, "ok").unwrap(), format!("{a}:ok"));
    }

    /// Always "ready", every accept() fails with EMFILE; counts the calls.
    struct Emfile(Arc<std::sync::atomic::AtomicU32>);

    impl Acceptor for Emfile {
        fn ready(&self) -> bool {
            true
        }
        fn accept(&self) -> io::Result<TcpStream> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Err(io::Error::from_raw_os_error(libc::EMFILE))
        }
    }

    #[test]
    fn accept_errors_keep_the_loop_alive_until_stopped() {
        // fd exhaustion: every accept() fails. The Go loop returns on the
        // first error and leaves a dead listener; this one must back off
        // (not spin), keep retrying, and still exit promptly once stopped.
        let calls = Arc::new(std::sync::atomic::AtomicU32::new(0));
        let stop = Arc::new(AtomicBool::new(false));
        let (flag, fake) = (stop.clone(), Emfile(calls.clone()));
        let t = thread::spawn(move || accept_loop(fake, 1, &flag, &echo()));
        thread::sleep(Duration::from_millis(300));
        assert!(
            !t.is_finished(),
            "accept loop gave up after an accept error"
        );
        let n = calls.load(Ordering::SeqCst);
        assert!(n >= 3, "kept retrying: {n} accept calls in 300 ms");
        assert!(
            n <= 20,
            "backs off instead of spinning: {n} calls in 300 ms"
        );
        stop.store(true, Ordering::SeqCst);
        let started = std::time::Instant::now();
        while !t.is_finished() {
            assert!(
                started.elapsed() < Duration::from_secs(2),
                "loop ignored stop"
            );
            thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn stop_needs_no_wake_up_connection() {
        // If stopping relied on connecting to the listener (which can fail,
        // e.g. under fd exhaustion), a failed wake-up would leave accept()
        // blocked with the port still bound after the map forgot it. The loop
        // must notice `stop` on its own.
        let ln = TcpListener::bind((LO, 0)).unwrap();
        let addr = ln.local_addr().unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let flag = stop.clone();
        let t = thread::spawn(move || accept_loop(ln, addr.port(), &flag, &echo()));
        thread::sleep(Duration::from_millis(100));
        stop.store(true, Ordering::SeqCst);
        let started = std::time::Instant::now();
        while !t.is_finished() {
            assert!(
                started.elapsed() < Duration::from_millis(500),
                "accept loop ignored stop without a wake-up"
            );
            thread::sleep(Duration::from_millis(10));
        }
        TcpListener::bind(addr).expect("port released");
    }

    #[test]
    fn accepted_connections_are_blocking() {
        // A nonblocking listener's accepted sockets inherit O_NONBLOCK on
        // BSD/macOS; handlers use blocking I/O, so the agent must clear it.
        let a = free_port();
        let (tx, rx) = std::sync::mpsc::channel();
        let tx = Mutex::new(tx);
        let probe: Handler = Arc::new(move |c: TcpStream, _| {
            let mut b = [0u8; 1];
            // Blocking: waits for the client's byte rather than WouldBlock.
            let r = (&c).read(&mut b).map(|_| b[0]);
            let _ = tx.lock().unwrap().send(r.map_err(|e| e.kind()));
        });
        let x = Exposes::new(LO, probe);
        x.reconcile(&[a as i64]);
        let mut c = TcpStream::connect((LO, a)).unwrap();
        thread::sleep(Duration::from_millis(150));
        c.write_all(b"z").unwrap();
        assert_eq!(rx.recv_timeout(Duration::from_secs(5)).unwrap(), Ok(b'z'));
    }

    #[test]
    fn closing_many_listeners_overlaps_their_waits() {
        // Each listener notices stop within one poll interval. Closed one
        // after another, each join waits out the rest of that listener's
        // interval (uniform, mean half an interval), so N closes cost about
        // N/2 intervals (≈1.5 s for 30; σ≈0.16 s), and a big enough
        // set_exposes could blow the host's 5 s reply deadline. Signalling all
        // before joining any makes it about one interval in total. The bound,
        // N/4 intervals, has several intervals of headroom for a loaded
        // runner and sits ~5σ below the sequential cost.
        const N: usize = 30;
        let ports: Vec<i64> = (0..N).map(|_| free_port() as i64).collect();
        let x = Exposes::new(LO, echo());
        x.reconcile(&ports);
        assert_eq!(x.ports().len(), N);
        let t = std::time::Instant::now();
        x.reconcile(&[]);
        let took = t.elapsed();
        assert!(x.ports().is_empty());
        let bound = Duration::from_millis((N as u64 * STOP_POLL_MS as u64) / 4);
        assert!(
            took < bound,
            "closing {N} listeners took {took:?} (bound {bound:?})"
        );
    }

    #[test]
    fn dropping_the_set_closes_its_listeners() {
        let (a, b) = (free_port(), free_port());
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64, b as i64]);
        drop(x);
        // Closed and released, not detached: both ports bind again at once.
        TcpListener::bind((LO, a)).expect("port a released on drop");
        TcpListener::bind((LO, b)).expect("port b released on drop");
    }

    #[test]
    fn out_of_range_ports_are_ignored() {
        let a = free_port();
        let x = Exposes::new(LO, echo());
        x.reconcile(&[0, -1, 65536, 1 << 40, a as i64]);
        assert_eq!(x.ports(), vec![a]);
    }

    #[test]
    fn a_port_that_fails_to_bind_is_skipped() {
        let taken = TcpListener::bind((LO, 0)).unwrap();
        let busy = taken.local_addr().unwrap().port();
        let a = free_port();
        let x = Exposes::new(LO, echo());
        x.reconcile(&[busy as i64, a as i64]);
        assert_eq!(x.ports(), vec![a], "busy port skipped, the other started");
        drop(taken);
    }

    #[test]
    fn concurrent_connections_each_get_a_handler() {
        let a = free_port();
        let x = Exposes::new(LO, echo());
        x.reconcile(&[a as i64]);
        let joins: Vec<_> = (0..8)
            .map(|i| thread::spawn(move || ask(a, &format!("c{i}")).unwrap()))
            .collect();
        for (i, j) in joins.into_iter().enumerate() {
            assert_eq!(j.join().unwrap(), format!("{a}:c{i}"));
        }
    }
}
