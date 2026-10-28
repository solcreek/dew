//! Per-connection request dispatch (mirrors handleConn / handleConnect in
//! cmd/dew-agent/main.go).

use crate::conn::Conn;
use crate::exec::{self, Cred};
use crate::gofmt;
use dew_proto::*;
use std::io::{self, ErrorKind};
use std::net::{SocketAddr, TcpStream, ToSocketAddrs};
use std::path::PathBuf;
use std::sync::OnceLock;
use std::thread;
use std::time::Duration;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

/// Whether this build enforces ExecRequest.confine. Acked in the set_token
/// reply; while false the host refuses `--confine` (fails closed) instead of
/// running the command unconfined. Flips to true with the confine shim.
pub const CONFINE_SUPPORTED: bool = false;

/// Agent-wide state shared by all connections.
pub struct Agent {
    token: OnceLock<String>,
    /// DEW_EXEC_USER: run commands as this user instead of root.
    pub exec_user: Option<String>,
    pub passwd: PathBuf,
}

impl Agent {
    pub fn new(exec_user: Option<String>) -> Agent {
        Agent {
            token: OnceLock::new(),
            exec_user,
            passwd: PathBuf::from("/etc/passwd"),
        }
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
                return reply(&mut conn, "set_exposes: not supported by this agent build");
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
                if write_frame(&mut conn, &exec::execute(&req, agent.cred())).is_err() {
                    return;
                }
            }
        }
    }
}

/// Dial like net.DialTimeout("tcp", addr, 5s), with Go's error wording.
fn dial(addr: &str) -> Result<TcpStream, String> {
    let addrs: Vec<SocketAddr> = match addr.to_socket_addrs() {
        Ok(a) => a.collect(),
        Err(e) => {
            let host = addr.rsplit_once(':').map_or(addr, |(h, _)| h);
            return Err(format!("dial tcp: lookup {host}: {e}"));
        }
    };
    let mut last = format!("dial tcp {addr}: no addresses");
    for a in addrs {
        match TcpStream::connect_timeout(&a, CONNECT_TIMEOUT) {
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
    let mut tcp = match dial(addr) {
        Ok(t) => t,
        Err(e) => return reply(&mut vs, &e),
    };
    if write_frame(
        &mut vs,
        &ConnectResponse {
            ok: true,
            ..Default::default()
        },
    )
    .is_err()
    {
        return;
    }
    let (Ok(mut vs_in), Ok(mut tcp_out)) = (vs.try_clone(), tcp.try_clone()) else {
        return;
    };
    let up = thread::spawn(move || {
        let _ = io::copy(&mut vs_in, &mut tcp_out);
    });
    let _ = io::copy(&mut tcp, &mut vs);
    let _ = up.join();
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

    #[test]
    fn empty_exec_user_means_unchanged() {
        assert_eq!(Agent::new(Some(String::new())).cred(), None);
        assert_eq!(Agent::new(None).cred(), None);
    }
}
