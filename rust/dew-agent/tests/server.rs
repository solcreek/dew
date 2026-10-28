//! Request dispatch over one connection (handle_conn), matched against the Go
//! agent's handleConn: the set_token handshake, fail-closed auth on every
//! request type, buffered exec reuse of a conn, streaming, and the connect
//! proxy. A socketpair stands in for the vsock conn.

use dew_agent::server::{handle_conn, Agent, CONFINE_SUPPORTED};
use dew_proto::*;
use serde_json::Value;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

const TOKEN: &str = "s3cret";

/// Serve one connection on a fresh socketpair; return the host end.
fn open(agent: &Arc<Agent>) -> UnixStream {
    let (a, h) = UnixStream::pair().unwrap();
    let agent = agent.clone();
    thread::spawn(move || handle_conn(a, &agent));
    h.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
    h
}

fn roundtrip<T: for<'de> serde::Deserialize<'de>>(
    agent: &Arc<Agent>,
    req: &impl serde::Serialize,
) -> T {
    let mut h = open(agent);
    write_frame(&mut h, req).unwrap();
    read_frame(&mut h).unwrap()
}

fn set_token(agent: &Arc<Agent>, token: &str) -> ConnectResponse {
    roundtrip(
        agent,
        &SetTokenRequest {
            kind: TYPE_SET_TOKEN.into(),
            token: token.into(),
        },
    )
}

fn ready_agent() -> Arc<Agent> {
    let a = Arc::new(Agent::new(None));
    assert!(set_token(&a, TOKEN).ok);
    a
}

fn exec_req(token: &str, script: &str) -> ExecRequest {
    ExecRequest {
        token: token.into(),
        command: "/bin/sh".into(),
        args: vec!["-c".into(), script.into()],
        ..Default::default()
    }
}

fn assert_closed(mut h: UnixStream) {
    let mut b = [0u8; 1];
    assert_eq!(h.read(&mut b).unwrap_or(0), 0, "conn should be closed");
}

// ---------------------------------------------------------------- set_token

#[test]
fn set_token_acks_once_and_reports_confine_support() {
    let a = Arc::new(Agent::new(None));
    let r = set_token(&a, TOKEN);
    assert!(r.ok);
    assert_eq!(r.confine, CONFINE_SUPPORTED);
    assert!(!r.confine, "phase 2 must not claim confine support");

    let again = set_token(&a, "attacker");
    assert!(!again.ok);
    assert_eq!(again.error, "token already set");
    assert!(
        a.authorized(TOKEN) && !a.authorized("attacker"),
        "first token sticks"
    );
}

#[test]
fn set_token_closes_the_conn() {
    let a = Arc::new(Agent::new(None));
    let mut h = open(&a);
    write_frame(
        &mut h,
        &SetTokenRequest {
            kind: TYPE_SET_TOKEN.into(),
            token: TOKEN.into(),
        },
    )
    .unwrap();
    let _: ConnectResponse = read_frame(&mut h).unwrap();
    assert_closed(h);
}

#[test]
fn empty_token_is_accepted_like_go() {
    // agentauth.IsAuthorized is tokenSet && given == expected, so an empty
    // token, once set, authorizes empty-token requests. Keep parity.
    let a = Arc::new(Agent::new(None));
    assert!(set_token(&a, "").ok);
    let r: ExecResponse = roundtrip(&a, &exec_req("", "echo ok"));
    assert_eq!(r.stdout, "ok\n");
}

// ---------------------------------------------------------------- auth

#[test]
fn every_request_is_rejected_before_set_token() {
    let a = Arc::new(Agent::new(None));
    let r: ExecResponse = roundtrip(&a, &exec_req("", "echo should-not-run"));
    assert_eq!(
        (r.exit_code, r.error.as_str(), r.stdout.as_str()),
        (-1, "unauthorized", "")
    );

    let r: ConnectResponse = roundtrip(
        &a,
        &ConnectRequest {
            kind: TYPE_CONNECT.into(),
            token: "".into(),
            addr: "127.0.0.1:1".into(),
        },
    );
    assert_eq!((r.ok, r.error.as_str()), (false, "unauthorized"));

    let r: ConnectResponse = roundtrip(
        &a,
        &SetExposesRequest {
            kind: TYPE_SET_EXPOSES.into(),
            token: "".into(),
            ports: Some(vec![3000]),
        },
    );
    assert_eq!((r.ok, r.error.as_str()), (false, "unauthorized"));
}

#[test]
fn wrong_token_is_rejected_and_closes() {
    let a = ready_agent();
    let mut h = open(&a);
    write_frame(&mut h, &exec_req("wrong", "echo nope")).unwrap();
    let r: ExecResponse = read_frame(&mut h).unwrap();
    assert_eq!(r.error, "unauthorized");
    assert_closed(h);
}

#[test]
fn ping_still_needs_the_token() {
    let a = ready_agent();
    let ping = |t: &str| -> ExecResponse {
        roundtrip(
            &a,
            &ExecRequest {
                token: t.into(),
                command: "ping".into(),
                ..Default::default()
            },
        )
    };
    assert_eq!(ping("wrong").error, "unauthorized");
    assert_eq!(ping(TOKEN).stdout, "pong");
}

#[test]
fn malformed_frame_is_treated_as_unauthorized_exec() {
    let a = ready_agent();
    let mut h = open(&a);
    h.write_all(&[0, 0, 0, 3]).unwrap();
    h.write_all(b"{x}").unwrap();
    let r: ExecResponse = read_frame(&mut h).unwrap();
    assert_eq!(r.error, "unauthorized");
}

#[test]
fn oversized_frame_drops_the_conn() {
    let a = ready_agent();
    let mut h = open(&a);
    h.write_all(&[0x7f, 0, 0, 0]).unwrap();
    assert_closed(h);
}

// ---------------------------------------------------------------- exec

#[test]
fn buffered_execs_can_share_one_conn() {
    let a = ready_agent();
    let mut h = open(&a);
    for i in 0..3 {
        write_frame(&mut h, &exec_req(TOKEN, &format!("echo {i}"))).unwrap();
        let r: ExecResponse = read_frame(&mut h).unwrap();
        assert_eq!(r.stdout, format!("{i}\n"));
    }
}

#[test]
fn host_closing_before_the_reply_ends_the_conn_quietly() {
    let a = ready_agent();
    let (agent_end, mut h) = UnixStream::pair().unwrap();
    let a2 = a.clone();
    let worker = thread::spawn(move || handle_conn(agent_end, &a2));
    write_frame(&mut h, &exec_req(TOKEN, "sleep 0.2; echo late")).unwrap();
    drop(h);
    worker.join().unwrap(); // returns on the failed reply write / EOF
}

#[test]
fn explicit_exec_type_is_dispatched_as_exec() {
    let a = ready_agent();
    let r: ExecResponse = roundtrip(
        &a,
        &ExecRequest {
            kind: TYPE_EXEC.into(),
            ..exec_req(TOKEN, "echo typed")
        },
    );
    assert_eq!(r.stdout, "typed\n");
}

#[test]
fn streaming_exec_owns_the_conn() {
    let a = ready_agent();
    let mut h = open(&a);
    write_frame(
        &mut h,
        &ExecRequest {
            stream: true,
            ..exec_req(TOKEN, "echo one; echo two >&2; exit 7")
        },
    )
    .unwrap();
    let mut lines = vec![];
    let done = loop {
        let v: Value = read_frame(&mut h).unwrap();
        if v.get("exit_code").is_some() {
            break serde_json::from_value::<ExecDone>(v).unwrap();
        }
        lines.push(serde_json::from_value::<OutputChunk>(v).unwrap());
    };
    assert_eq!(lines.len(), 2);
    assert_eq!(done.exit_code, 7);
    assert_closed(h);
}

#[test]
fn exec_user_runs_commands_as_that_user() {
    let (uid, gid) = unsafe { (libc::getuid(), libc::getgid()) };
    let passwd =
        std::env::temp_dir().join(format!("dew-agent-server-passwd-{}", std::process::id()));
    std::fs::write(&passwd, format!("me:x:{uid}:{gid}::/:/bin/sh\n")).unwrap();
    let mut agent = Agent::new(Some("me".into()));
    agent.passwd = passwd.clone();
    let a = Arc::new(agent);
    assert!(set_token(&a, TOKEN).ok);
    let r: ExecResponse = roundtrip(&a, &exec_req(TOKEN, "id -u"));
    assert_eq!(r.stdout, format!("{uid}\n"));

    // An unknown user leaves the exec unchanged, as the Go agent's
    // setExecUser does when user.Lookup fails.
    let mut agent = Agent::new(Some("ghost".into()));
    agent.passwd = passwd.clone();
    let a = Arc::new(agent);
    assert!(set_token(&a, TOKEN).ok);
    let r: ExecResponse = roundtrip(&a, &exec_req(TOKEN, "echo ran"));
    assert_eq!(r.stdout, "ran\n");
    let _ = std::fs::remove_file(passwd);
}

// ---------------------------------------------------------------- set_exposes

#[test]
fn set_exposes_validates_then_reports_unsupported() {
    let a = ready_agent();
    let mut h = open(&a);
    let bad = br#"{"type":"set_exposes","token":"s3cret","ports":"nope"}"#;
    h.write_all(&(bad.len() as u32).to_be_bytes()).unwrap();
    h.write_all(bad).unwrap();
    let r: ConnectResponse = read_frame(&mut h).unwrap();
    assert_eq!(r.error, "bad set_exposes request");

    let r: ConnectResponse = roundtrip(
        &a,
        &SetExposesRequest {
            kind: TYPE_SET_EXPOSES.into(),
            token: TOKEN.into(),
            ports: Some(vec![3000]),
        },
    );
    assert!(!r.ok);
    assert!(r.error.starts_with("set_exposes:"), "{}", r.error);
}

// ---------------------------------------------------------------- connect

fn connect(agent: &Arc<Agent>, addr: &str) -> (UnixStream, ConnectResponse) {
    let mut h = open(agent);
    write_frame(
        &mut h,
        &ConnectRequest {
            kind: TYPE_CONNECT.into(),
            token: TOKEN.into(),
            addr: addr.into(),
        },
    )
    .unwrap();
    let r = read_frame(&mut h).unwrap();
    (h, r)
}

#[test]
fn connect_proxies_bytes_both_ways() {
    let srv = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = srv.local_addr().unwrap().to_string();
    thread::spawn(move || {
        let (mut s, _) = srv.accept().unwrap();
        let mut buf = [0u8; 5];
        s.read_exact(&mut buf).unwrap();
        s.write_all(&buf.to_ascii_uppercase()).unwrap();
    });
    let a = ready_agent();
    let (mut h, r) = connect(&a, &addr);
    assert!(r.ok, "{}", r.error);
    h.write_all(b"hello").unwrap();
    let mut got = [0u8; 5];
    h.read_exact(&mut got).unwrap();
    assert_eq!(&got, b"HELLO");
}

#[test]
fn connect_refused_uses_go_wording() {
    // Bind then drop to get a port that is almost certainly closed.
    let port = TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let a = ready_agent();
    let (h, r) = connect(&a, &format!("127.0.0.1:{port}"));
    assert!(!r.ok);
    assert_eq!(
        r.error,
        format!("dial tcp 127.0.0.1:{port}: connect: connection refused")
    );
    assert_closed(h);
}

#[test]
fn connect_unresolvable_host_reports_lookup() {
    let a = ready_agent();
    let (_, r) = connect(&a, "no-such-host.invalid:80");
    assert!(!r.ok);
    assert!(
        r.error
            .starts_with("dial tcp: lookup no-such-host.invalid:"),
        "{}",
        r.error
    );
}

#[test]
fn connect_without_port_is_an_error() {
    let a = ready_agent();
    let (_, r) = connect(&a, "127.0.0.1");
    assert!(!r.ok);
    assert!(r.error.starts_with("dial tcp"), "{}", r.error);
}
