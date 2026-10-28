//! Buffered and streaming exec (mirrors executeCommand / executeStreaming in
//! cmd/dew-agent/main.go).

use crate::conn::Conn;
use crate::env::{env_pairs, exec_env, look_path, DEFAULT_PATH};
use crate::gofmt;
use dew_proto::{write_frame, ExecDone, ExecRequest, ExecResponse, InputChunk, OutputChunk};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::thread;
use std::time::Duration;

/// Default exec timeout when the request sets none (or a non-positive one).
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// Longest stdout/stderr line sent as one OutputChunk. Longer lines go out in
/// pieces of this size. (The Go agent's bufio.Scanner stops reading a pipe at
/// a 64 KiB line, which stalls the command; splitting avoids that.)
pub const MAX_CHUNK: u64 = 64 * 1024;

const CONFINE_UNSUPPORTED: &str = "confine: not supported by this agent build";
const TTY_UNSUPPORTED: &str = "tty: not supported by this agent build";

/// uid/gid the agent runs commands as (DEW_EXEC_USER), resolved per exec.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Cred {
    pub uid: u32,
    pub gid: u32,
}

/// Look `name` up in an /etc/passwd-format file, as Go's pure-Go user.Lookup
/// does (by user name only).
pub fn lookup_user(passwd: &str, name: &str) -> Option<Cred> {
    std::fs::read_to_string(passwd).ok()?.lines().find_map(|l| {
        let f: Vec<&str> = l.split(':').collect();
        (f.len() >= 4 && f[0] == name).then(|| {
            Some(Cred {
                uid: f[2].parse().ok()?,
                gid: f[3].parse().ok()?,
            })
        })?
    })
}

fn timeout_of(req: &ExecRequest) -> Duration {
    if req.timeout_ms > 0 {
        Duration::from_millis(req.timeout_ms as u64)
    } else {
        DEFAULT_TIMEOUT
    }
}

fn timeout_error(t: Duration) -> String {
    format!("timeout after {}", gofmt::duration(t))
}

/// Exit code as Go's ExitError.ExitCode reports it: -1 for a signal.
fn exit_code(s: &ExitStatus) -> i64 {
    s.code().map_or(-1, i64::from)
}

/// Build the command the Go agent would run: argv[0] resolved against the
/// agent's own PATH, the guestenv exec env, the working dir, DEW_EXEC_USER.
fn build(req: &ExecRequest, cred: Option<Cred>) -> Result<Command, String> {
    let agent_path = std::env::var("PATH").unwrap_or_else(|_| DEFAULT_PATH.into());
    let path = look_path(&req.command, &agent_path)?;
    if !req.dir.is_empty() {
        if let Err(e) = std::fs::metadata(&req.dir) {
            return Err(format!("chdir {}: {}", req.dir, gofmt::errno_text(&e)));
        }
    }
    let mut cmd = Command::new(&path);
    cmd.arg0(&req.command).args(&req.args);
    if !req.dir.is_empty() {
        cmd.current_dir(&req.dir);
    }
    let base: Vec<String> = std::env::vars().map(|(k, v)| format!("{k}={v}")).collect();
    cmd.env_clear().envs(env_pairs(&exec_env(&base, &req.env)));
    if let Some(c) = cred {
        cmd.uid(c.uid).gid(c.gid);
    }
    // Own process group, so a timeout can kill the whole tree. (The Go agent
    // kills only the direct child; a grandchild still holding the output
    // pipe then keeps the exec running past its deadline.)
    cmd.process_group(0);
    Ok(cmd)
}

fn spawn_error(cmd: &Command, e: &io::Error) -> String {
    format!(
        "fork/exec {}: {}",
        cmd.get_program().to_string_lossy(),
        gofmt::errno_text(e)
    )
}

/// Waits for a child with an optional deadline. On expiry the child's whole
/// process group gets SIGKILL. The kill can never hit a recycled pid: the
/// group id is the leader's pid, which stays reserved until the leader is
/// reaped, and `wait` marks the leader exited (under the lock, observed with
/// WNOWAIT) before reaping it. Callers drain the output pipes *before*
/// `wait`, so a deadline that fires while a grandchild holds a pipe still
/// lands on a live group.
struct Deadline {
    pid: libc::pid_t,
    exited: Mutex<bool>,
    fired: AtomicBool,
}

impl Deadline {
    fn arm(child: &Child, timeout: Option<Duration>) -> (Arc<Deadline>, Option<mpsc::Sender<()>>) {
        let d = Arc::new(Deadline {
            pid: child.id() as libc::pid_t,
            exited: Mutex::new(false),
            fired: AtomicBool::new(false),
        });
        let Some(timeout) = timeout else {
            return (d, None);
        };
        let (tx, rx) = mpsc::channel::<()>();
        let k = d.clone();
        thread::spawn(move || {
            if let Err(mpsc::RecvTimeoutError::Timeout) = rx.recv_timeout(timeout) {
                let exited = k.exited.lock().unwrap();
                if !*exited {
                    k.fired.store(true, Ordering::SeqCst);
                    unsafe { libc::kill(-k.pid, libc::SIGKILL) };
                }
            }
        });
        (d, Some(tx))
    }

    fn wait(&self, child: &mut Child, cancel: Option<mpsc::Sender<()>>) -> io::Result<ExitStatus> {
        loop {
            let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
            let rc = unsafe {
                libc::waitid(
                    libc::P_PID,
                    self.pid as libc::id_t,
                    &mut info,
                    libc::WEXITED | libc::WNOWAIT,
                )
            };
            if rc == 0 {
                break;
            }
            let e = io::Error::last_os_error();
            if e.kind() != io::ErrorKind::Interrupted {
                return Err(e);
            }
        }
        *self.exited.lock().unwrap() = true;
        if let Some(tx) = cancel {
            let _ = tx.send(());
        }
        child.wait()
    }

    fn fired(&self) -> bool {
        self.fired.load(Ordering::SeqCst)
    }
}

fn read_all(mut r: impl Read + Send + 'static) -> thread::JoinHandle<Vec<u8>> {
    thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = r.read_to_end(&mut buf);
        buf
    })
}

/// Run a non-streaming exec and collect its output (executeCommand).
pub fn execute(req: &ExecRequest, cred: Option<Cred>) -> ExecResponse {
    let fail = |error: String| ExecResponse {
        exit_code: -1,
        error,
        ..Default::default()
    };
    if req.command == "ping" {
        return ExecResponse {
            stdout: "pong".into(),
            ..Default::default()
        };
    }
    if req.confine.as_ref().is_some_and(|c| c.is_set()) {
        return fail(CONFINE_UNSUPPORTED.into());
    }
    let timeout = timeout_of(req);
    let mut cmd = match build(req, cred) {
        Ok(c) => c,
        Err(e) => return fail(e),
    };
    cmd.stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return fail(spawn_error(&cmd, &e)),
    };
    let (deadline, cancel) = Deadline::arm(&child, Some(timeout));
    let out = read_all(child.stdout.take().unwrap());
    let err = read_all(child.stderr.take().unwrap());
    // Drain before reaping: see Deadline.
    let (stdout, stderr) = (
        out.join().unwrap_or_default(),
        err.join().unwrap_or_default(),
    );
    let status = deadline.wait(&mut child, cancel);

    let mut resp = ExecResponse {
        stdout: String::from_utf8_lossy(&stdout).into_owned(),
        ..Default::default()
    };
    if deadline.fired() {
        resp.exit_code = -1;
        resp.error = timeout_error(timeout);
        return resp;
    }
    match status {
        Ok(s) if s.success() => {}
        // Go's cmd.Output returns stderr only inside an ExitError.
        Ok(s) => {
            resp.exit_code = exit_code(&s);
            resp.stderr = String::from_utf8_lossy(&stderr).into_owned();
        }
        Err(e) => {
            resp.exit_code = -1;
            resp.error = e.to_string();
        }
    }
    resp
}

/// Send one pipe's output as OutputChunk frames, a line at a time. Like the
/// Go agent's bufio.Scanner: a trailing "\r\n" or "\n" becomes "\n", and a
/// final unterminated line gets one.
fn pump<C: Conn>(
    pipe: impl Read + Send + 'static,
    stream: &'static str,
    w: Arc<Mutex<C>>,
) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        let mut r = BufReader::with_capacity(MAX_CHUNK as usize, pipe);
        let mut buf = Vec::with_capacity(256);
        loop {
            buf.clear();
            match r.by_ref().take(MAX_CHUNK).read_until(b'\n', &mut buf) {
                Ok(0) | Err(_) => return,
                Ok(_) => {}
            }
            if buf.last() == Some(&b'\n') {
                buf.pop();
                if buf.last() == Some(&b'\r') {
                    buf.pop();
                }
                buf.push(b'\n');
            } else if (buf.len() as u64) < MAX_CHUNK {
                buf.push(b'\n'); // last line without a newline (EOF)
            }
            let chunk = OutputChunk {
                stream: stream.into(),
                data: String::from_utf8_lossy(&buf).into_owned(),
            };
            if write_frame(&mut *w.lock().unwrap(), &chunk).is_err() {
                return;
            }
        }
    })
}

/// Feed InputChunk frames from the host into the child's stdin until EOF, a
/// read error or a closed conn (executeStreaming's stdin goroutine).
fn feed_stdin<C: Conn>(mut conn: C, mut stdin: impl Write + Send + 'static) {
    thread::spawn(move || loop {
        let Ok(chunk) = dew_proto::read_frame::<InputChunk>(&mut conn) else {
            return;
        };
        if !chunk.data.is_empty() && stdin.write_all(chunk.data.as_bytes()).is_err() {
            return;
        }
        if chunk.eof {
            return;
        }
    });
}

/// Run a streaming exec on `conn`, which it owns for its lifetime: output
/// frames, then one ExecDone, then the conn is shut down.
pub fn execute_streaming<C: Conn>(conn: C, req: &ExecRequest, cred: Option<Cred>) {
    let writer = match conn.try_clone() {
        Ok(w) => Arc::new(Mutex::new(w)),
        Err(_) => return,
    };
    let done = |d: ExecDone| {
        let _ = write_frame(&mut *writer.lock().unwrap(), &d);
        conn.shutdown();
    };
    let fail = |error: &str| {
        done(ExecDone {
            exit_code: -1,
            error: error.into(),
        })
    };
    if req.tty {
        return fail(TTY_UNSUPPORTED);
    }
    if req.confine.as_ref().is_some_and(|c| c.is_set()) {
        return fail(CONFINE_UNSUPPORTED);
    }
    // An interactive session runs until stdin closes or the process exits,
    // so only batch streams get the exec timeout.
    let timeout = (!req.stdin).then(|| timeout_of(req));
    let mut cmd = match build(req, cred) {
        Ok(c) => c,
        Err(e) => return fail(&e),
    };
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
    cmd.stdin(if req.stdin {
        Stdio::piped()
    } else {
        Stdio::null()
    });
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return fail(&spawn_error(&cmd, &e)),
    };
    let (deadline, cancel) = Deadline::arm(&child, timeout);
    if let Some(stdin) = child.stdin.take() {
        match conn.try_clone() {
            Ok(reader) => feed_stdin(reader, stdin),
            Err(_) => drop(stdin),
        }
    }
    let a = pump(child.stdout.take().unwrap(), "stdout", writer.clone());
    let b = pump(child.stderr.take().unwrap(), "stderr", writer.clone());
    let _ = a.join();
    let _ = b.join();
    let status = deadline.wait(&mut child, cancel);

    let mut d = match status {
        Ok(s) => ExecDone {
            exit_code: if s.success() { 0 } else { exit_code(&s) },
            error: String::new(),
        },
        Err(e) => ExecDone {
            exit_code: -1,
            error: e.to_string(),
        },
    };
    if let (true, Some(t)) = (deadline.fired(), timeout) {
        d = ExecDone {
            exit_code: -1,
            error: timeout_error(t),
        };
    }
    done(d);
}
