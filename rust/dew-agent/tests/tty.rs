//! TTY exec over a socketpair, matched against the Go agent's runPTY: a real
//! pty (isatty, window size, controlling terminal), base64 frames both ways,
//! resizes, and exit status passthrough.

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use dew_agent::exec::execute_streaming;
use dew_proto::*;
use serde_json::Value;
use std::os::unix::net::UnixStream;
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

const LIMIT: Duration = Duration::from_secs(20);

struct Session {
    /// Every OutputChunk, in order.
    chunks: Vec<OutputChunk>,
    done: ExecDone,
}

impl Session {
    /// The pty output: every chunk's data, base64-decoded and joined.
    fn bytes(&self) -> Vec<u8> {
        self.chunks
            .iter()
            .flat_map(|c| STANDARD.decode(&c.data).expect("TTY output must be base64"))
            .collect()
    }
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.bytes()).into_owned()
    }
}

fn tty_req(script: &str, rows: u16, cols: u16) -> ExecRequest {
    ExecRequest {
        command: "/bin/sh".into(),
        args: vec!["-c".into(), script.into()],
        stream: true,
        stdin: true,
        tty: true,
        rows,
        cols,
        ..Default::default()
    }
}

fn data(s: &str) -> InputChunk {
    InputChunk {
        data: STANDARD.encode(s),
        ..Default::default()
    }
}

fn winch(rows: u16, cols: u16) -> InputChunk {
    InputChunk {
        winch: true,
        rows,
        cols,
        ..Default::default()
    }
}

/// Run a TTY exec, send `input` frames, and collect frames until ExecDone.
fn run(req: ExecRequest, input: Vec<InputChunk>) -> Session {
    let (agent, mut host) = UnixStream::pair().unwrap();
    thread::spawn(move || execute_streaming(agent, &req, None));
    let mut w = host.try_clone().unwrap();
    thread::spawn(move || {
        for chunk in input {
            if write_frame(&mut w, &chunk).is_err() {
                return;
            }
            // Let the shell consume each line before the next arrives.
            thread::sleep(Duration::from_millis(50));
        }
    });
    let (tx, rx) = mpsc::channel();
    thread::spawn(move || {
        let mut chunks = vec![];
        loop {
            let v: Value = read_frame(&mut host).expect("frame decodes");
            if v.get("exit_code").is_some() {
                let _ = tx.send((chunks, serde_json::from_value::<ExecDone>(v).unwrap()));
                return;
            }
            chunks.push(serde_json::from_value::<OutputChunk>(v).unwrap());
        }
    });
    let (chunks, done) = rx.recv_timeout(LIMIT).expect("ExecDone within the limit");
    Session { chunks, done }
}

// Port of TestExecuteStreaming_TTY (cmd/dew-agent/pty_test.go).
#[test]
fn allocates_a_real_tty_with_the_requested_size() {
    let s = run(
        tty_req("stty size; test -t 0 && echo IS_TTY", 24, 80),
        vec![],
    );
    let out = s.text();
    assert!(out.contains("IS_TTY"), "guest does not see a tty: {out:?}");
    assert!(out.contains("24 80"), "window size not applied: {out:?}");
    assert_eq!(s.done.exit_code, 0);
}

#[test]
fn output_is_one_base64_stdout_stream() {
    let s = run(tty_req("echo out; echo err >&2", 24, 80), vec![]);
    assert!(!s.chunks.is_empty());
    assert!(
        s.chunks.iter().all(|c| c.stream == "stdout"),
        "pty merges stderr into stdout"
    );
    let out = s.text();
    assert!(out.contains("out") && out.contains("err"), "{out:?}");
}

#[test]
fn output_is_binary_safe() {
    let s = run(tty_req("printf '\\377\\001\\002'", 24, 80), vec![]);
    let b = s.bytes();
    assert!(b.windows(3).any(|w| w == [0xFF, 0x01, 0x02]), "{b:?}");
}

#[test]
fn zero_size_leaves_the_window_unset() {
    // The kernel-level 0x0 is asserted in pty::tests; here, how a 0x0 window
    // looks to a command: BSD/GNU stty print "0 0", BusyBox stty (the guest)
    // treats it as unavailable and fails with "stty: standard input".
    let s = run(tty_req("stty size", 0, 0), vec![]);
    let out = s.text();
    assert!(
        out.contains("0 0") || out.contains("stty: standard input"),
        "a size was applied: {out:?}"
    );
}

#[test]
fn has_a_controlling_terminal_and_own_session() {
    // /dev/tty opens only with a controlling terminal; its size is ours.
    let s = run(tty_req("stty size </dev/tty", 33, 101), vec![]);
    assert!(s.text().contains("33 101"), "{:?}", s.text());
}

#[test]
fn stdin_reaches_the_command() {
    let s = run(
        tty_req("read line; echo got=$line", 24, 80),
        vec![data("hello\n")],
    );
    assert!(s.text().contains("got=hello"), "{:?}", s.text());
    assert_eq!(s.done.exit_code, 0);
}

#[test]
fn interactive_shell_session_and_exit_code() {
    let req = ExecRequest {
        args: vec![],
        ..tty_req("", 24, 80)
    };
    let s = run(req, vec![data("echo sum=$((40+2))\n"), data("exit 3\n")]);
    assert!(s.text().contains("sum=42"), "{:?}", s.text());
    assert_eq!((s.done.exit_code, s.done.error.as_str()), (3, ""));
}

#[test]
fn resize_applies_before_later_input() {
    let s = run(
        tty_req("read x; stty size", 24, 80),
        vec![winch(50, 132), data("\n")],
    );
    assert!(s.text().contains("50 132"), "{:?}", s.text());
}

#[test]
fn resize_frame_data_is_ignored_like_go() {
    let mut w = winch(40, 100);
    w.data = STANDARD.encode("zzz\n");
    let s = run(
        tty_req("read x; echo got=$x", 24, 80),
        vec![w, data("ok\n")],
    );
    assert!(s.text().contains("got=ok"), "{:?}", s.text());
    assert!(!s.text().contains("zzz"), "{:?}", s.text());
}

#[test]
fn undecodable_input_is_skipped() {
    let bad = InputChunk {
        data: "!!not base64!!".into(),
        ..Default::default()
    };
    let s = run(
        tty_req("read x; echo got=$x", 24, 80),
        vec![bad, data("ok\n")],
    );
    assert!(s.text().contains("got=ok"), "{:?}", s.text());
}

#[test]
fn dir_and_env_apply_in_tty_mode() {
    let mut req = tty_req("pwd; echo foo=$FOO", 24, 80);
    req.dir = "/".into();
    req.env = vec!["FOO=bar".into()];
    let out = run(req, vec![]).text();
    assert!(out.contains("/\r\n") || out.starts_with("/\r"), "{out:?}");
    assert!(out.contains("foo=bar"), "{out:?}");
}

#[test]
fn signalled_command_reports_minus_one() {
    let s = run(tty_req("kill -9 $$", 24, 80), vec![]);
    assert_eq!((s.done.exit_code, s.done.error.as_str()), (-1, ""));
}

#[test]
fn spawn_failure_is_exec_done() {
    let req = ExecRequest {
        command: "no-such-cmd-dew".into(),
        args: vec![],
        ..tty_req("", 24, 80)
    };
    let s = run(req, vec![]);
    assert!(s.chunks.is_empty());
    assert_eq!(s.done.exit_code, -1);
    assert!(
        s.done.error.contains("executable file not found"),
        "{}",
        s.done.error
    );
}

#[test]
fn large_output_arrives_whole() {
    // More than several READ_CHUNKs; nothing may be lost before EIO/EOF.
    let n = 300_000;
    let s = run(
        tty_req(&format!("head -c {n} /dev/zero | tr '\\0' x"), 24, 80),
        vec![],
    );
    let xs = s.bytes().iter().filter(|&&b| b == b'x').count();
    assert_eq!(xs, n);
    assert!(s.chunks.len() > 1);
}
