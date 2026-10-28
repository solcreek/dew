//! TTY exec: the command runs on a pseudo-terminal (mirrors runPTY in
//! cmd/dew-agent/main.go). One merged output stream, isatty true, job
//! control. Terminal bytes are binary, so InputChunk/OutputChunk `data` is
//! standard base64 in this mode.

use crate::conn::Conn;
use crate::exec::{self, Cred};
use crate::pty;
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use dew_proto::{read_frame, write_frame, ExecDone, ExecRequest, InputChunk, OutputChunk};
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::fd::OwnedFd;
use std::os::unix::process::CommandExt;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::thread;

/// Bytes read from the pty per OutputChunk (the Go agent's buffer size).
pub const READ_CHUNK: usize = 32 * 1024;

fn stdio(fd: &OwnedFd) -> io::Result<Stdio> {
    fd.try_clone().map(Stdio::from)
}

/// Run `req` on a new pty and stream it over `conn`; returns the ExecDone to
/// send. There is no exec timeout: a TTY session is interactive.
pub(crate) fn run<C: Conn>(
    conn: &C,
    writer: &Arc<Mutex<C>>,
    req: &ExecRequest,
    cred: Option<Cred>,
) -> ExecDone {
    let fail = |error: String| ExecDone {
        exit_code: -1,
        error,
    };
    let p = match pty::open(req.rows, req.cols) {
        Ok(p) => p,
        Err(e) => return fail(format!("open pty: {e}")),
    };
    let mut cmd = match exec::build_base(req, cred) {
        Ok(c) => c,
        Err(e) => return fail(e),
    };
    match (stdio(&p.slave), stdio(&p.slave), stdio(&p.slave)) {
        (Ok(i), Ok(o), Ok(e)) => {
            cmd.stdin(i).stdout(o).stderr(e);
        }
        _ => return fail(format!("dup pty: {}", io::Error::last_os_error())),
    }
    unsafe { cmd.pre_exec(pty::become_session_leader_on_stdin) };
    let spawned = cmd.spawn();
    let spawn_err = spawned.as_ref().err().map(|e| exec::spawn_error(&cmd, e));
    // Drop every parent-side copy of the slave (Command holds three), so the
    // master reports EOF/EIO once the child's side is gone.
    drop(cmd);
    drop(p.slave);
    let mut child = match spawned {
        Ok(c) => c,
        Err(_) => return fail(spawn_err.unwrap_or_default()),
    };
    let mut master = Some(p.master);
    // Guards the hangup signal against a reaped (recyclable) pid.
    let (session, _) = exec::Deadline::arm(&child, None);

    // host -> pty: stdin bytes and window-size changes.
    let m = master.as_ref().unwrap();
    if let (Ok(reader), Ok(input)) = (conn.try_clone(), m.try_clone()) {
        feed(reader, input, session.clone());
    }

    // pty -> host: raw bytes, base64-framed.
    let mut buf = vec![0u8; READ_CHUNK];
    while let Some(m) = master.as_mut() {
        match m.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                let chunk = OutputChunk {
                    stream: "stdout".into(),
                    data: STANDARD.encode(&buf[..n]),
                };
                if write_frame(&mut *writer.lock().unwrap(), &chunk).is_err() {
                    // The host is gone. Hang the session up as a terminal
                    // does when its line drops: SIGHUP, and close the master
                    // so a command ignoring SIGHUP still gets EIO.
                    session.signal_group(libc::SIGHUP);
                    master = None;
                }
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
            // Linux reports EIO once the last slave fd is closed.
            Err(_) => break,
        }
    }

    let status = session.wait(&mut child, None);
    // As the Go agent (deferred ptmx.Close), the master closes after Wait.
    drop(master);
    match status {
        Ok(s) if s.success() => ExecDone::default(),
        Ok(s) => ExecDone {
            exit_code: exec::exit_code(&s),
            error: String::new(),
        },
        Err(e) => fail(e.to_string()),
    }
}

/// Apply InputChunk frames to the pty until EOF or a read error. A resize
/// frame carries nothing else (its data is ignored, as in the Go agent);
/// undecodable base64 and failed writes are skipped.
///
/// The conn failing before an EOF frame means the host went away, so the
/// session is hung up (SIGHUP to its process group). The Go agent just stops
/// reading, and an idle shell then waits on the pty forever. After an EOF
/// frame the host may half-close and keep reading output: no hangup.
fn feed<C: Conn>(mut conn: C, mut master: File, session: Arc<exec::Deadline>) {
    thread::spawn(move || loop {
        let Ok(chunk) = read_frame::<InputChunk>(&mut conn) else {
            session.signal_group(libc::SIGHUP);
            return;
        };
        if chunk.winch {
            let _ = pty::set_size(&master, chunk.rows, chunk.cols);
            continue;
        }
        if !chunk.data.is_empty() {
            if let Ok(bytes) = STANDARD.decode(chunk.data.as_bytes()) {
                let _ = master.write_all(&bytes);
            }
        }
        if chunk.eof {
            return;
        }
    });
}
