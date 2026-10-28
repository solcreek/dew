//! `dew up --expose-host` end to end inside the agent: set_exposes over a
//! conn, a guest client connecting to an exposed port, and the reverse dial
//! to a fake host on a socketpair (the real one is vsock CID 2, port 1025).
//! Matched against the Go agent's startExposeForwarders / handleExposeConn.

use dew_agent::server::{handle_conn, Agent};
use dew_proto::*;
use std::fs::File;
use std::io::{Read, Write};
use std::net::{IpAddr, Ipv4Addr, TcpListener, TcpStream};
use std::os::fd::OwnedFd;
use std::os::unix::net::UnixStream;
use std::sync::{mpsc, Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

const TOKEN: &str = "s3cret";
const LO: IpAddr = IpAddr::V4(Ipv4Addr::LOCALHOST);

/// The host side of each reverse dial the agent makes, in order.
type HostEnds = mpsc::Receiver<UnixStream>;

fn agent_with_fake_host(reverse_timeout: Duration) -> (Arc<Agent>, HostEnds, Arc<Mutex<u32>>) {
    let (tx, rx) = mpsc::channel::<UnixStream>();
    let tx = Mutex::new(tx);
    let dials = Arc::new(Mutex::new(0u32));
    let count = dials.clone();
    let mut a = Agent::new(None);
    a.expose_ip = LO; // 127.0.0.2 is not configured on macOS
    a.reverse_timeout = reverse_timeout;
    a.host_dialer = Arc::new(move || {
        *count.lock().unwrap() += 1;
        let (agent_end, host_end) = UnixStream::pair()?;
        tx.lock().unwrap().send(host_end).unwrap();
        Ok(File::from(OwnedFd::from(agent_end)))
    });
    let a = Arc::new(a);
    let (c, h) = UnixStream::pair().unwrap();
    let a2 = a.clone();
    thread::spawn(move || handle_conn(c, &a2));
    let mut h = h;
    write_frame(
        &mut h,
        &SetTokenRequest {
            kind: TYPE_SET_TOKEN.into(),
            token: TOKEN.into(),
        },
    )
    .unwrap();
    assert!(read_frame::<ConnectResponse>(&mut h).unwrap().ok);
    (a, rx, dials)
}

fn set_exposes(agent: &Arc<Agent>, ports: Option<Vec<i64>>) -> ConnectResponse {
    let (c, mut h) = UnixStream::pair().unwrap();
    let a = agent.clone();
    thread::spawn(move || handle_conn(c, &a));
    write_frame(
        &mut h,
        &SetExposesRequest {
            kind: TYPE_SET_EXPOSES.into(),
            token: TOKEN.into(),
            ports,
        },
    )
    .unwrap();
    read_frame(&mut h).unwrap()
}

fn free_port() -> u16 {
    TcpListener::bind((LO, 0))
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn connect(port: u16) -> TcpStream {
    let c = TcpStream::connect((LO, port)).expect("exposed port accepts");
    c.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    c
}

/// The next reverse dial: its request, with the host end to answer on.
fn next_dial(host: &HostEnds) -> (ReverseDialRequest, UnixStream) {
    let mut h = host
        .recv_timeout(Duration::from_secs(5))
        .expect("agent dialed the host");
    h.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    let req: ReverseDialRequest = read_frame(&mut h).unwrap();
    (req, h)
}

fn read_eof(c: &mut TcpStream) -> bool {
    let mut b = [0u8; 1];
    matches!(c.read(&mut b), Ok(0))
}

#[test]
fn exposed_connection_is_tunnelled_to_the_host() {
    let (a, host, _) = agent_with_fake_host(Duration::from_secs(5));
    let port = free_port();
    assert!(set_exposes(&a, Some(vec![port as i64])).ok);

    let mut guest = connect(port);
    let (req, mut h) = next_dial(&host);
    assert_eq!(req.kind, "reverse_dial");
    assert_eq!(req.token, TOKEN, "host validates the session token");
    assert_eq!(req.port, port as i64, "host dials 127.0.0.1:<this port>");
    write_frame(
        &mut h,
        &ReverseDialResponse {
            ok: true,
            ..Default::default()
        },
    )
    .unwrap();

    guest.write_all(b"ping").unwrap();
    let mut b = [0u8; 4];
    h.read_exact(&mut b).unwrap();
    assert_eq!(&b, b"ping", "guest -> host");
    h.write_all(b"pong").unwrap();
    guest.read_exact(&mut b).unwrap();
    assert_eq!(&b, b"pong", "host -> guest");
}

#[test]
fn host_refusal_closes_the_guest_connection() {
    let (a, host, _) = agent_with_fake_host(Duration::from_secs(5));
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    let mut guest = connect(port);
    let (_, mut h) = next_dial(&host);
    write_frame(
        &mut h,
        &ReverseDialResponse {
            ok: false,
            error: "port not exposed".into(),
        },
    )
    .unwrap();
    assert!(read_eof(&mut guest), "guest conn must be closed on refusal");
}

#[test]
fn silent_host_times_out_and_closes_the_guest_connection() {
    let (a, host, _) = agent_with_fake_host(Duration::from_millis(200));
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    let mut guest = connect(port);
    let (_, _h) = next_dial(&host); // never answers, keeps the conn open
    let t = Instant::now();
    assert!(read_eof(&mut guest));
    assert!(
        t.elapsed() < Duration::from_secs(3),
        "not bounded: {:?}",
        t.elapsed()
    );
}

#[test]
fn trickled_host_answer_is_bounded_by_one_deadline() {
    // The budget covers the whole response frame (Go's ReadJSONTimeout), not
    // each read: a host dribbling a valid OK one byte per 100 ms must not keep
    // the guest connection past a 300 ms budget, nor get the tunnel opened.
    let (a, host, _) = agent_with_fake_host(Duration::from_millis(300));
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    let mut guest = connect(port);
    let (_, mut h) = next_dial(&host);
    let frame = {
        let body = serde_json::to_vec(&ReverseDialResponse {
            ok: true,
            ..Default::default()
        })
        .unwrap();
        let mut f = (body.len() as u32).to_be_bytes().to_vec();
        f.extend(body);
        f
    };
    thread::spawn(move || {
        for b in frame {
            if h.write_all(&[b]).is_err() {
                return;
            }
            thread::sleep(Duration::from_millis(100));
        }
        thread::sleep(Duration::from_secs(10)); // keep the host end open
    });
    let t = Instant::now();
    assert!(
        read_eof(&mut guest),
        "tunnel opened despite the expired deadline"
    );
    assert!(
        t.elapsed() < Duration::from_millis(1200),
        "not bounded: {:?}",
        t.elapsed()
    );
}

#[test]
fn failed_host_dial_closes_the_guest_connection() {
    let mut a = Agent::new(None);
    a.expose_ip = LO;
    a.host_dialer = Arc::new(|| Err(std::io::Error::other("no host")));
    let a = Arc::new(a);
    let (c, mut h) = UnixStream::pair().unwrap();
    let a2 = a.clone();
    thread::spawn(move || handle_conn(c, &a2));
    write_frame(
        &mut h,
        &SetTokenRequest {
            kind: TYPE_SET_TOKEN.into(),
            token: TOKEN.into(),
        },
    )
    .unwrap();
    let _: ConnectResponse = read_frame(&mut h).unwrap();
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    assert!(read_eof(&mut connect(port)));
}

#[test]
fn each_guest_connection_gets_its_own_dial() {
    let (a, host, dials) = agent_with_fake_host(Duration::from_secs(5));
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    let _g1 = connect(port);
    let _g2 = connect(port);
    let (_, _h1) = next_dial(&host);
    let (_, _h2) = next_dial(&host);
    assert_eq!(*dials.lock().unwrap(), 2);
}

#[test]
fn set_exposes_reconciles_and_nil_closes_everything() {
    let (a, _host, _) = agent_with_fake_host(Duration::from_secs(5));
    let (p1, p2) = (free_port(), free_port());
    assert!(set_exposes(&a, Some(vec![p1 as i64, p2 as i64])).ok);
    let mut both = vec![p1, p2];
    both.sort();
    assert_eq!(a.exposes().ports(), both);

    assert!(set_exposes(&a, Some(vec![p2 as i64])).ok);
    assert_eq!(a.exposes().ports(), vec![p2]);
    assert!(TcpStream::connect((LO, p1)).is_err(), "dropped port closed");

    // Go encodes a nil slice as "ports":null; that is the empty set.
    assert!(set_exposes(&a, None).ok);
    assert!(a.exposes().ports().is_empty());
}

#[test]
fn unauthorized_set_exposes_changes_nothing() {
    let (a, _host, _) = agent_with_fake_host(Duration::from_secs(5));
    let port = free_port();
    set_exposes(&a, Some(vec![port as i64]));
    let (c, mut h) = UnixStream::pair().unwrap();
    let a2 = a.clone();
    thread::spawn(move || handle_conn(c, &a2));
    write_frame(
        &mut h,
        &SetExposesRequest {
            kind: TYPE_SET_EXPOSES.into(),
            token: "wrong".into(),
            ports: None,
        },
    )
    .unwrap();
    let r: ConnectResponse = read_frame(&mut h).unwrap();
    assert_eq!(r.error, "unauthorized");
    assert_eq!(a.exposes().ports(), vec![port], "listeners untouched");
}
