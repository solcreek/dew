//! Behaviour of buffered and streaming exec, matched against what the Go
//! agent (cmd/dew-agent) does for the same request. Runs on any Unix: the
//! exec paths take a generic Conn, and a socketpair stands in for vsock.

use dew_agent::exec::{execute, execute_streaming, lookup_user, Cred, MAX_CHUNK};
use dew_proto::*;
use serde_json::Value;
use std::io::Write;
use std::os::unix::net::UnixStream;
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

fn sh(script: &str) -> ExecRequest {
    ExecRequest {
        command: "/bin/sh".into(),
        args: vec!["-c".into(), script.into()],
        ..Default::default()
    }
}

// ---------------------------------------------------------------- buffered

#[test]
fn ping_answers_without_forking() {
    let r = execute(
        &ExecRequest {
            command: "ping".into(),
            ..Default::default()
        },
        None,
    );
    assert_eq!(
        (r.exit_code, r.stdout.as_str(), r.error.as_str()),
        (0, "pong", "")
    );
}

#[test]
fn success_returns_stdout_and_drops_stderr() {
    // Go's cmd.Output only surfaces stderr inside an ExitError.
    let r = execute(&sh("echo out; echo err >&2"), None);
    assert_eq!(r.exit_code, 0);
    assert_eq!(r.stdout, "out\n");
    assert_eq!(r.stderr, "");
    assert_eq!(r.error, "");
}

#[test]
fn failure_returns_exit_code_and_stderr() {
    let r = execute(&sh("echo out; echo boom >&2; exit 3"), None);
    assert_eq!(r.exit_code, 3);
    assert_eq!(r.stdout, "out\n");
    assert_eq!(r.stderr, "boom\n");
    assert_eq!(r.error, "");
}

#[test]
fn signalled_process_reports_minus_one_without_error() {
    let r = execute(&sh("kill -9 $$"), None);
    assert_eq!(r.exit_code, -1);
    assert_eq!(r.error, "");
}

#[test]
fn missing_command_uses_go_wording() {
    let r = execute(
        &ExecRequest {
            command: "no-such-cmd-dew".into(),
            ..Default::default()
        },
        None,
    );
    assert_eq!(r.exit_code, -1);
    assert_eq!(
        r.error,
        "exec: \"no-such-cmd-dew\": executable file not found in $PATH"
    );
}

#[test]
fn missing_absolute_command_reports_fork_exec() {
    let r = execute(
        &ExecRequest {
            command: "/no/such/bin".into(),
            ..Default::default()
        },
        None,
    );
    assert_eq!(r.exit_code, -1);
    assert_eq!(r.error, "fork/exec /no/such/bin: no such file or directory");
}

#[test]
fn bare_name_resolves_on_agent_path() {
    let r = execute(
        &ExecRequest {
            command: "echo".into(),
            args: vec!["hi".into()],
            ..Default::default()
        },
        None,
    );
    assert_eq!((r.exit_code, r.stdout.as_str()), (0, "hi\n"));
}

#[test]
fn request_path_does_not_change_resolution() {
    // The Go agent resolves argv[0] against its own PATH, never the child's.
    let mut req = ExecRequest {
        command: "echo".into(),
        args: vec!["ok".into()],
        ..Default::default()
    };
    req.env = vec!["PATH=/nonexistent".into()];
    let r = execute(&req, None);
    assert_eq!((r.exit_code, r.stdout.as_str()), (0, "ok\n"));
}

#[test]
fn dir_and_env_are_applied() {
    let mut req = sh("pwd; echo \"$FOO\"; echo \"$BAR\"");
    req.dir = "/".into();
    req.env = vec!["FOO=one".into(), "BAR=a=b".into(), "FOO=two".into()];
    let r = execute(&req, None);
    assert_eq!(
        r.stdout, "/\ntwo\na=b\n",
        "later env entries win, values keep '='"
    );
}

#[test]
fn child_env_always_has_a_path() {
    let r = execute(&sh("echo \"$PATH\""), None);
    assert!(!r.stdout.trim().is_empty());
}

#[test]
fn missing_dir_uses_go_wording() {
    let mut req = sh("true");
    req.dir = "/no/such/dir".into();
    let r = execute(&req, None);
    assert_eq!(r.exit_code, -1);
    assert_eq!(r.error, "chdir /no/such/dir: no such file or directory");
}

#[test]
fn timeout_kills_and_reports_go_duration() {
    let mut req = sh("echo started; sleep 5");
    req.timeout_ms = 200;
    let t = Instant::now();
    let r = execute(&req, None);
    assert!(
        t.elapsed() < Duration::from_secs(3),
        "not killed: {:?}",
        t.elapsed()
    );
    assert_eq!(r.exit_code, -1);
    assert_eq!(r.error, "timeout after 200ms");
    assert_eq!(r.stdout, "started\n", "output before the deadline is kept");
}

#[test]
fn timeout_kills_grandchildren_holding_the_pipe() {
    // The shell exits at once, but a background child keeps stdout open. The
    // deadline must still end the exec (the Go agent waits for the child).
    let mut req = sh("sleep 5 & echo bg");
    req.timeout_ms = 300;
    let t = Instant::now();
    let r = execute(&req, None);
    assert!(
        t.elapsed() < Duration::from_secs(3),
        "pipe holder survived: {:?}",
        t.elapsed()
    );
    assert_eq!((r.exit_code, r.error.as_str()), (-1, "timeout after 300ms"));
    assert_eq!(r.stdout, "bg\n");
}

#[test]
fn non_positive_timeout_means_default() {
    let mut req = sh("echo ok");
    req.timeout_ms = -5;
    assert_eq!(execute(&req, None).stdout, "ok\n");
}

#[test]
fn stdin_is_empty_for_buffered_exec() {
    let r = execute(&sh("cat; echo done"), None);
    assert_eq!(r.stdout, "done\n");
}

#[test]
fn invalid_utf8_is_replaced_like_encoding_json() {
    let r = execute(&sh("printf '\\377ok'"), None);
    assert_eq!(r.stdout, "\u{FFFD}ok");
}

#[test]
fn confine_fails_closed_until_supported() {
    let mut req = sh("true");
    req.confine = Some(Confinement {
        read_only_root: true,
        ..Default::default()
    });
    let r = execute(&req, None);
    assert_eq!(r.exit_code, -1);
    assert!(r.error.starts_with("confine:"), "{}", r.error);
}

#[test]
fn empty_confine_spec_runs_normally() {
    let mut req = sh("echo ok");
    req.confine = Some(Confinement::default());
    assert_eq!(execute(&req, None).stdout, "ok\n");
}

// ---------------------------------------------------------------- users

fn passwd_file(body: &str) -> tempfile_lite::Path {
    tempfile_lite::write("passwd", body)
}

#[test]
fn lookup_user_reads_passwd_format() {
    let f = passwd_file("root:x:0:0:root:/root:/bin/sh\n# comment\ndew:x:1000:1001::/home/dew:/bin/sh\nbad:x:nope:1:::\n");
    assert_eq!(
        lookup_user(f.as_str(), "dew"),
        Some(Cred {
            uid: 1000,
            gid: 1001
        })
    );
    assert_eq!(
        lookup_user(f.as_str(), "root"),
        Some(Cred { uid: 0, gid: 0 })
    );
    assert_eq!(lookup_user(f.as_str(), "bad"), None, "non-numeric uid");
    assert_eq!(lookup_user(f.as_str(), "nobody-here"), None);
    assert_eq!(lookup_user("/no/such/passwd", "dew"), None);
}

#[test]
fn exec_user_credentials_are_applied() {
    // Dropping to our own uid/gid needs no privilege, but exercises the path.
    let (uid, gid) = unsafe { (libc::getuid(), libc::getgid()) };
    let r = execute(&sh("id -u; id -g"), Some(Cred { uid, gid }));
    assert_eq!(r.stdout, format!("{uid}\n{gid}\n"));
}

// ---------------------------------------------------------------- streaming

struct Stream {
    chunks: Vec<OutputChunk>,
    done: ExecDone,
    host: UnixStream,
}

impl Stream {
    fn joined(&self, stream: &str) -> String {
        self.chunks
            .iter()
            .filter(|c| c.stream == stream)
            .map(|c| c.data.as_str())
            .collect()
    }
}

/// Run a streaming exec against a socketpair, optionally sending stdin
/// frames, and collect every frame up to ExecDone. Bounded by `limit`.
fn stream(req: ExecRequest, input: Vec<InputChunk>, limit: Duration) -> Stream {
    let (agent, mut host) = UnixStream::pair().unwrap();
    thread::spawn(move || execute_streaming(agent, &req, None));
    let mut writer = host.try_clone().unwrap();
    thread::spawn(move || {
        for chunk in input {
            if write_frame(&mut writer, &chunk).is_err() {
                return;
            }
        }
    });
    let (tx, rx) = mpsc::channel();
    let mut reader = host.try_clone().unwrap();
    thread::spawn(move || {
        let mut chunks = Vec::new();
        loop {
            let v: Value = read_frame(&mut reader).expect("frame decodes");
            if v.get("exit_code").is_some() {
                let done: ExecDone = serde_json::from_value(v).unwrap();
                let _ = tx.send((chunks, done));
                return;
            }
            chunks.push(serde_json::from_value::<OutputChunk>(v).unwrap());
        }
    });
    let (chunks, done) = rx.recv_timeout(limit).expect("ExecDone within the limit");
    let _ = host.flush();
    Stream { chunks, done, host }
}

const LIMIT: Duration = Duration::from_secs(20);

fn streaming(script: &str) -> ExecRequest {
    ExecRequest {
        stream: true,
        ..sh(script)
    }
}

#[test]
fn stream_sends_lines_then_exec_done() {
    let s = stream(
        streaming("echo a; echo b >&2; echo c; exit 4"),
        vec![],
        LIMIT,
    );
    assert_eq!(s.joined("stdout"), "a\nc\n");
    assert_eq!(s.joined("stderr"), "b\n");
    assert!(s
        .chunks
        .iter()
        .all(|c| c.data.ends_with('\n') && c.data.matches('\n').count() == 1));
    assert_eq!((s.done.exit_code, s.done.error.as_str()), (4, ""));
}

#[test]
fn stream_conn_is_closed_after_exec_done() {
    let mut s = stream(streaming("true"), vec![], LIMIT);
    let mut buf = [0u8; 1];
    // macOS rejects setsockopt (EINVAL) on an already shut-down socket.
    let _ = s.host.set_read_timeout(Some(Duration::from_secs(5)));
    assert_eq!(
        std::io::Read::read(&mut s.host, &mut buf).unwrap(),
        0,
        "expected EOF"
    );
}

#[test]
fn stream_normalises_line_endings_like_bufio_scanner() {
    let s = stream(
        streaming("printf 'crlf\\r\\nlast-no-newline'"),
        vec![],
        LIMIT,
    );
    assert_eq!(s.joined("stdout"), "crlf\nlast-no-newline\n");
}

#[test]
fn stream_splits_lines_longer_than_max_chunk() {
    let n = MAX_CHUNK as usize * 2 + 10;
    let s = stream(
        streaming(&format!("head -c {n} /dev/zero | tr '\\0' x; echo")),
        vec![],
        LIMIT,
    );
    let out = s.joined("stdout");
    assert_eq!(out.len(), n + 1);
    assert!(out.trim_end_matches('\n').bytes().all(|b| b == b'x'));
    assert!(s.chunks.len() >= 3, "a long line must not be one frame");
    assert!(s
        .chunks
        .iter()
        .all(|c| c.data.len() as u64 <= MAX_CHUNK + 1));
    assert_eq!(s.done.exit_code, 0);
}

#[test]
fn stream_concurrent_stdout_stderr_never_interleaves_frames() {
    // The case that corrupted 16% of streams in the Go agent before #44.
    let n = 5000;
    let s = stream(
        streaming(&format!(
            "i=0; while [ $i -lt {n} ]; do echo out$i; echo err$i >&2; i=$((i+1)); done"
        )),
        vec![],
        LIMIT,
    );
    let out: Vec<_> = s.chunks.iter().filter(|c| c.stream == "stdout").collect();
    let err: Vec<_> = s.chunks.iter().filter(|c| c.stream == "stderr").collect();
    assert_eq!((out.len(), err.len()), (n, n));
    assert_eq!(
        out[n - 1].data,
        format!("out{}\n", n - 1),
        "per-stream order is kept"
    );
    assert_eq!(err[0].data, "err0\n");
    assert_eq!(s.done.exit_code, 0);
}

#[test]
fn stream_feeds_stdin_until_eof() {
    let mut req = ExecRequest {
        command: "cat".into(),
        stream: true,
        stdin: true,
        ..Default::default()
    };
    req.args = vec![];
    let input = vec![
        InputChunk {
            data: "hi\n".into(),
            ..Default::default()
        },
        InputChunk {
            data: "there\n".into(),
            ..Default::default()
        },
        InputChunk {
            eof: true,
            ..Default::default()
        },
    ];
    let s = stream(req, input, LIMIT);
    assert_eq!(s.joined("stdout"), "hi\nthere\n");
    assert_eq!(s.done.exit_code, 0);
}

#[test]
fn stream_without_stdin_gets_empty_stdin() {
    let s = stream(streaming("cat; echo done"), vec![], LIMIT);
    assert_eq!(s.joined("stdout"), "done\n");
}

#[test]
fn stream_batch_timeout_kills_and_reports() {
    let mut req = streaming("echo started; sleep 5");
    req.timeout_ms = 300;
    let t = Instant::now();
    let s = stream(req, vec![], LIMIT);
    assert!(
        t.elapsed() < Duration::from_secs(4),
        "not killed: {:?}",
        t.elapsed()
    );
    assert_eq!(s.joined("stdout"), "started\n");
    assert_eq!(
        (s.done.exit_code, s.done.error.as_str()),
        (-1, "timeout after 300ms")
    );
}

#[test]
fn stream_interactive_session_ignores_exec_timeout() {
    let req = ExecRequest {
        stdin: true,
        timeout_ms: 100,
        ..streaming("sleep 0.5; echo survived")
    };
    let s = stream(
        req,
        vec![InputChunk {
            eof: true,
            ..Default::default()
        }],
        LIMIT,
    );
    assert_eq!(s.joined("stdout"), "survived\n");
    assert_eq!((s.done.exit_code, s.done.error.as_str()), (0, ""));
}

#[test]
fn stream_start_failure_is_exec_done() {
    let req = ExecRequest {
        command: "no-such-cmd-dew".into(),
        stream: true,
        ..Default::default()
    };
    let s = stream(req, vec![], LIMIT);
    assert!(s.chunks.is_empty());
    assert_eq!(s.done.exit_code, -1);
    assert!(
        s.done.error.contains("executable file not found"),
        "{}",
        s.done.error
    );
}

#[test]
fn stream_tty_and_confine_fail_closed_until_supported() {
    let tty = ExecRequest {
        tty: true,
        stdin: true,
        ..streaming("true")
    };
    let s = stream(tty, vec![], LIMIT);
    assert!(s.done.error.starts_with("tty:"), "{}", s.done.error);

    let mut confined = streaming("true");
    confined.confine = Some(Confinement {
        user: "nobody".into(),
        ..Default::default()
    });
    let s = stream(confined, vec![], LIMIT);
    assert_eq!(s.done.exit_code, -1);
    assert!(s.done.error.starts_with("confine:"), "{}", s.done.error);
}

/// Minimal temp-file helper (keeps the crate free of dev-dependencies).
mod tempfile_lite {
    pub struct Path(std::path::PathBuf);
    impl Path {
        pub fn as_str(&self) -> &str {
            self.0.to_str().unwrap()
        }
    }
    impl Drop for Path {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }
    pub fn write(name: &str, body: &str) -> Path {
        use std::sync::atomic::{AtomicU32, Ordering};
        static N: AtomicU32 = AtomicU32::new(0);
        let p = std::env::temp_dir().join(format!(
            "dew-agent-test-{}-{}-{name}",
            std::process::id(),
            N.fetch_add(1, Ordering::SeqCst)
        ));
        std::fs::write(&p, body).unwrap();
        Path(p)
    }
}
