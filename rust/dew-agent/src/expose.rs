//! Guest-side listeners for `dew up --expose-host` (mirrors
//! startExposeForwarders / acceptExposeConns in cmd/dew-agent/main.go).
//!
//! For each exposed host port the guest listens on 127.0.0.2:<port>
//! (host.lo.internal resolves there, so it can't collide with 127.0.0.1
//! services in the same VM) and hands each accepted connection to a handler,
//! which tunnels it to the host.

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::net::{IpAddr, SocketAddr, TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::Duration;

/// The guest loopback alias exposed host ports listen on.
pub const EXPOSE_IP: &str = "127.0.0.2";

/// Called on its own thread for each accepted connection, with the port.
pub type Handler = Arc<dyn Fn(TcpStream, u16) + Send + Sync>;

struct Listener {
    addr: SocketAddr,
    stop: Arc<AtomicBool>,
    thread: JoinHandle<()>,
}

impl Listener {
    /// Stop accepting and release the port. A blocked accept() is woken by
    /// connecting to it (shutdown(2) on a listening socket doesn't wake it
    /// on every OS); returns once the listener is closed.
    fn close(self) {
        self.stop.store(true, Ordering::SeqCst);
        if TcpStream::connect_timeout(&self.addr, Duration::from_secs(1)).is_ok() {
            let _ = self.thread.join();
        } else {
            eprintln!("expose: could not wake listener on {}", self.addr);
        }
    }
}

/// The set of running expose listeners.
pub struct Exposes {
    ip: IpAddr,
    handler: Handler,
    listeners: Mutex<BTreeMap<u16, Listener>>,
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
        for port in gone {
            if let Some(l) = running.remove(&port) {
                l.close();
            }
        }
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
        let addr = ln.local_addr()?;
        let stop = Arc::new(AtomicBool::new(false));
        let (flag, handler) = (stop.clone(), self.handler.clone());
        let thread = thread::spawn(move || accept_loop(ln, port, &flag, &handler));
        Ok(Listener { addr, stop, thread })
    }
}

fn accept_loop(ln: TcpListener, port: u16, stop: &AtomicBool, handler: &Handler) {
    loop {
        let accepted = ln.accept();
        if stop.load(Ordering::SeqCst) {
            return; // drops the wake-up conn and the listener
        }
        match accepted {
            Ok((conn, _)) => {
                let h = handler.clone();
                thread::spawn(move || h(conn, port));
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
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
    use std::sync::mpsc;

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

    #[test]
    fn unchanged_ports_are_left_running() {
        // A connection held open across a reconcile that keeps its port must
        // survive (the listener isn't restarted).
        let a = free_port();
        let (tx, rx) = mpsc::channel::<()>();
        let rx = Arc::new(Mutex::new(rx));
        let hold: Handler = Arc::new(move |mut c: TcpStream, _| {
            let _ = rx.lock().unwrap().recv_timeout(Duration::from_secs(5));
            let _ = c.write_all(b"held");
        });
        let x = Exposes::new(LO, hold);
        x.reconcile(&[a as i64]);
        let mut c = TcpStream::connect((LO, a)).unwrap();
        thread::sleep(Duration::from_millis(50));
        x.reconcile(&[a as i64, a as i64]);
        tx.send(()).unwrap();
        let mut out = String::new();
        c.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        c.read_to_string(&mut out).unwrap();
        assert_eq!(out, "held");
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
