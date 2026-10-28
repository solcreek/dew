//! Per-connection request dispatch (mirrors handleConn / handleConnect in
//! cmd/dew-agent/main.go).

use crate::conn::Conn;
use crate::exec::{self, Cred};
use crate::expose::{reverse_dial, Exposes, Handler, HostDialer, EXPOSE_IP, REVERSE_DIAL_TIMEOUT};
use crate::gofmt;
use dew_proto::*;
use std::io::{self, ErrorKind};
use std::net::{IpAddr, SocketAddr, TcpStream, ToSocketAddrs};
use std::path::PathBuf;
use std::sync::{mpsc, Arc, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

/// Whether this build enforces ExecRequest.confine. Acked in the set_token
/// reply; while false the host refuses `--confine` (fails closed) instead of
/// running the command unconfined. Flips to true with the confine shim.
pub const CONFINE_SUPPORTED: bool = false;

/// Agent-wide state shared by all connections.
pub struct Agent {
    /// Shared with the expose handler, which presents it on reverse dials.
    token: Arc<OnceLock<String>>,
    /// DEW_EXEC_USER: run commands as this user instead of root.
    pub exec_user: Option<String>,
    pub passwd: PathBuf,
    /// Where expose listeners bind (127.0.0.2 in the guest).
    pub expose_ip: IpAddr,
    /// How an exposed connection reaches the host.
    pub host_dialer: HostDialer,
    /// How long a reverse dial waits for the host's answer.
    pub reverse_timeout: Duration,
    /// Created on the first set_exposes, from the fields above.
    exposes: OnceLock<Exposes>,
}

/// The guest's reverse-forward dial: vsock to the host on port 1025.
fn vsock_host_dialer() -> HostDialer {
    #[cfg(target_os = "linux")]
    return Arc::new(|| crate::vsock::dial(libc::VMADDR_CID_HOST, REVERSE_FORWARD_PORT));
    #[cfg(not(target_os = "linux"))]
    return Arc::new(|| {
        Err(io::Error::new(
            ErrorKind::Unsupported,
            "vsock is Linux-only",
        ))
    });
}

impl Agent {
    pub fn new(exec_user: Option<String>) -> Agent {
        Agent {
            token: Arc::new(OnceLock::new()),
            exec_user,
            passwd: PathBuf::from("/etc/passwd"),
            expose_ip: EXPOSE_IP.parse().expect("valid expose IP"),
            host_dialer: vsock_host_dialer(),
            reverse_timeout: REVERSE_DIAL_TIMEOUT,
            exposes: OnceLock::new(),
        }
    }

    /// The expose listener set, created on first use.
    pub fn exposes(&self) -> &Exposes {
        self.exposes.get_or_init(|| {
            let (token, dial, timeout) = (
                self.token.clone(),
                self.host_dialer.clone(),
                self.reverse_timeout,
            );
            let handler: Handler = Arc::new(move |tcp, port| {
                let t = token.get().map(String::as_str).unwrap_or_default();
                reverse_dial(tcp, port, t, &dial, timeout);
            });
            Exposes::new(self.expose_ip, handler)
        })
    }

    /// Fail closed: nothing is authorized until the host has sent set_token.
    /// Same predicate as internal/agentauth.IsAuthorized.
    pub fn authorized(&self, given: &str) -> bool {
        self.token.get().is_some_and(|t| t == given)
    }

    fn cred(&self) -> Option<Cred> {
        let user = self.exec_user.as_deref().filter(|u| !u.is_empty())?;
        exec::lookup_user(self.passwd.to_str()?, user)
    }
}

fn reply(conn: &mut impl io::Write, error: &str) {
    let _ = write_frame(
        conn,
        &ConnectResponse {
            error: error.into(),
            ..Default::default()
        },
    );
}

/// Serve one connection until the peer hangs up or a request ends it.
/// set_token, set_exposes and connect end the connection; a streaming exec
/// owns it until ExecDone; buffered execs can repeat on the same conn.
pub fn handle_conn<C: Conn>(mut conn: C, agent: &Agent) {
    loop {
        let Ok(data) = read_frame_bytes(&mut conn) else {
            return;
        };
        let env: Envelope = serde_json::from_slice(&data).unwrap_or_default();
        match env.kind.as_str() {
            TYPE_SET_TOKEN => {
                let req: SetTokenRequest = serde_json::from_slice(&data).unwrap_or_default();
                if agent.token.set(req.token).is_ok() {
                    let ack = ConnectResponse {
                        ok: true,
                        confine: CONFINE_SUPPORTED,
                        ..Default::default()
                    };
                    let _ = write_frame(&mut conn, &ack);
                } else {
                    reply(&mut conn, "token already set");
                }
                return;
            }
            TYPE_SET_EXPOSES => {
                let Ok(req) = serde_json::from_slice::<SetExposesRequest>(&data) else {
                    return reply(&mut conn, "bad set_exposes request");
                };
                if !agent.authorized(&req.token) {
                    return reply(&mut conn, "unauthorized");
                }
                // The full desired set: a nil/empty list closes every listener.
                agent.exposes().reconcile(&req.ports.unwrap_or_default());
                let ok = ConnectResponse {
                    ok: true,
                    ..Default::default()
                };
                let _ = write_frame(&mut conn, &ok);
                return;
            }
            TYPE_CONNECT => {
                let req: ConnectRequest = serde_json::from_slice(&data).unwrap_or_default();
                if !agent.authorized(&req.token) {
                    return reply(&mut conn, "unauthorized");
                }
                return handle_connect(conn, &req.addr);
            }
            _ => {
                let req: ExecRequest = serde_json::from_slice(&data).unwrap_or_default();
                if !agent.authorized(&req.token) {
                    let resp = ExecResponse {
                        exit_code: -1,
                        error: "unauthorized".into(),
                        ..Default::default()
                    };
                    let _ = write_frame(&mut conn, &resp);
                    return;
                }
                if req.stream {
                    return exec::execute_streaming(conn, &req, agent.cred());
                }
                if write_frame(
                    &mut conn,
                    &exec::execute_as(&req, agent.cred(), agent.exec_user.as_deref()),
                )
                .is_err()
                {
                    return;
                }
            }
        }
    }
}

/// Dial like net.DialTimeout("tcp", addr, 5s), with Go's error wording.
fn dial(addr: &str) -> Result<TcpStream, String> {
    dial_within(addr, CONNECT_TIMEOUT, |a| {
        a.to_socket_addrs().map(|it| it.collect())
    })
}

/// Shortest slice of the budget an address gets while others remain, as
/// Go's net.partialDeadline (saneMinimum).
const MIN_PER_ADDR: Duration = Duration::from_secs(2);

/// net.DialTimeout semantics: one `budget` covers the lookup and every
/// connection attempt. The lookup runs on a helper thread (getaddrinfo can't
/// be cancelled; a stalled one is abandoned), and each address gets the
/// remaining time split across the addresses left, but at least
/// MIN_PER_ADDR of it.
fn dial_within<R>(addr: &str, budget: Duration, resolve: R) -> Result<TcpStream, String>
where
    R: FnOnce(String) -> io::Result<Vec<SocketAddr>> + Send + 'static,
{
    let deadline = Instant::now() + budget;
    let host = addr.rsplit_once(':').map_or(addr, |(h, _)| h).to_string();
    let (tx, rx) = mpsc::channel();
    let owned = addr.to_string();
    thread::spawn(move || {
        let _ = tx.send(resolve(owned));
    });
    let addrs = match rx.recv_timeout(budget) {
        Ok(Ok(a)) => a,
        Ok(Err(e)) => return Err(format!("dial tcp: lookup {host}: {e}")),
        Err(_) => return Err(format!("dial tcp: lookup {host}: i/o timeout")),
    };
    let mut last = format!("dial tcp {addr}: no addresses");
    let total = addrs.len() as u32;
    for (i, a) in addrs.into_iter().enumerate() {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(format!("dial tcp {a}: i/o timeout"));
        }
        let share = (remaining / (total - i as u32)).max(MIN_PER_ADDR.min(remaining));
        match TcpStream::connect_timeout(&a, share) {
            Ok(s) => return Ok(s),
            Err(e) if e.kind() == ErrorKind::TimedOut => {
                last = format!("dial tcp {a}: i/o timeout")
            }
            Err(e) => last = format!("dial tcp {a}: connect: {}", gofmt::errno_text(&e)),
        }
    }
    Err(last)
}

/// Dial a TCP address inside the guest and proxy bytes both ways until both
/// directions finish (no half-close, as in the Go agent).
fn handle_connect<C: Conn>(mut vs: C, addr: &str) {
    let tcp = match dial(addr) {
        Ok(t) => t,
        Err(e) => return reply(&mut vs, &e),
    };
    let ok = ConnectResponse {
        ok: true,
        ..Default::default()
    };
    if write_frame(&mut vs, &ok).is_ok() {
        crate::conn::proxy(vs, tcp);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Same table as internal/agentauth/auth_test.go.
    #[test]
    fn authorization_fails_closed() {
        let cases: &[(&str, Option<&str>, &str, bool)] = &[
            ("token not set, no given — reject", None, "", false),
            (
                "token not set, attacker guesses — reject",
                None,
                "guess",
                false,
            ),
            ("token set, no given — reject", Some("secret"), "", false),
            (
                "token set, mismatch — reject",
                Some("secret"),
                "wrong",
                false,
            ),
            ("token set, match — allow", Some("secret"), "secret", true),
        ];
        for (name, expected, given, want) in cases {
            let a = Agent::new(None);
            if let Some(t) = expected {
                a.token.set(t.to_string()).unwrap();
            }
            assert_eq!(a.authorized(given), *want, "{name}");
        }
    }

    fn timed<T>(f: impl FnOnce() -> T) -> (T, Duration) {
        let t = std::time::Instant::now();
        (f(), t.elapsed())
    }

    #[test]
    fn stalled_lookup_is_bounded_by_the_dial_budget() {
        let (r, took) = timed(|| {
            dial_within("slow.example:80", Duration::from_millis(300), |_| {
                thread::sleep(Duration::from_secs(3));
                Ok(vec![])
            })
        });
        assert!(
            took < Duration::from_secs(1),
            "lookup not bounded: {took:?}"
        );
        assert_eq!(r.unwrap_err(), "dial tcp: lookup slow.example: i/o timeout");
    }

    #[test]
    fn several_unreachable_addresses_share_one_budget() {
        // Non-routable addresses either hang or fail fast; either way the
        // whole dial must end within the single budget, not one per address.
        let unroutable: Vec<SocketAddr> = vec![
            "10.255.255.1:9".parse().unwrap(),
            "10.255.255.2:9".parse().unwrap(),
            "10.255.255.3:9".parse().unwrap(),
        ];
        let (r, took) = timed(|| {
            dial_within("multi:9", Duration::from_millis(400), move |_| {
                Ok(unroutable)
            })
        });
        assert!(r.is_err());
        assert!(
            took < Duration::from_millis(1500),
            "budget exceeded: {took:?}"
        );
    }

    #[test]
    fn lookup_error_uses_go_wording() {
        let r = dial_within("bad.example:80", Duration::from_secs(1), |_| {
            Err(io::Error::other("no such host"))
        });
        assert_eq!(r.unwrap_err(), "dial tcp: lookup bad.example: no such host");
    }

    #[test]
    fn empty_lookup_result_is_an_error() {
        let r = dial_within("none.example:80", Duration::from_secs(1), |_| Ok(vec![]));
        assert!(r.unwrap_err().starts_with("dial tcp"));
    }

    #[test]
    fn defaults_target_the_guest() {
        let a = Agent::new(None);
        assert_eq!(a.expose_ip.to_string(), "127.0.0.2");
        assert_eq!(a.reverse_timeout, Duration::from_secs(5));
    }

    // Off Linux there is no vsock: the default dialer must fail cleanly, not
    // pretend. (On Linux it really dials CID 2, which tests don't attempt.)
    #[cfg(not(target_os = "linux"))]
    #[test]
    fn default_host_dialer_is_unsupported_off_linux() {
        let e = (Agent::new(None).host_dialer)().unwrap_err();
        assert_eq!(e.kind(), ErrorKind::Unsupported);
    }

    #[test]
    fn empty_exec_user_means_unchanged() {
        assert_eq!(Agent::new(Some(String::new())).cred(), None);
        assert_eq!(Agent::new(None).cred(), None);
    }
}
