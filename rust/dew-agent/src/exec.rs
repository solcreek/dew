//! Buffered and streaming exec (mirrors executeCommand / executeStreaming in
//! the retired Go agent's main.go).

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

/// Streaming execs never run confined: the shim runs on the batch path and
/// the host refuses --confine with --stream. (The Go agent's streaming path
/// ignores the spec and runs the command unconfined; this fails closed.)
const CONFINE_STREAMING: &str = "confine: not supported on the streaming path";

/// uid/gid the agent runs commands as (DEW_EXEC_USER), resolved per exec.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Cred {
    pub uid: u32,
    pub gid: u32,
}

/// Look `name` up in an /etc/passwd-format file by user name, with Go's
/// os/user parsing rules (the Go agent's setExecUser uses user.Lookup). Ids
/// that don't fit a uid_t leave the exec unchanged.
pub fn lookup_user(passwd: &str, name: &str) -> Option<Cred> {
    let u = crate::passwd::user_by_name(passwd, name).ok()??;
    Some(Cred {
        uid: u.uid.parse().ok()?,
        gid: u.gid.parse().ok()?,
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
pub(crate) fn exit_code(s: &ExitStatus) -> i64 {
    s.code().map_or(-1, i64::from)
}

/// The command in its own process group (see Deadline), for pipe execs.
fn build(req: &ExecRequest, cred: Option<Cred>) -> Result<Command, String> {
    let mut cmd = build_base(req, cred)?;
    // Own process group, so a timeout can kill the whole tree. (The Go agent
    // kills only the direct child; a grandchild still holding the output
    // pipe then keeps the exec running past its deadline.)
    cmd.process_group(0);
    Ok(cmd)
}

/// Build the command the Go agent would run: argv[0] resolved against the
/// agent's own PATH, the exec env (env.rs), the working dir, DEW_EXEC_USER.
/// No process group: a TTY exec makes its own session instead, and setsid
/// fails for a process that already leads a group.
pub(crate) fn build_base(req: &ExecRequest, cred: Option<Cred>) -> Result<Command, String> {
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
    Ok(cmd)
}

/// The agent binary a confined exec re-runs as the shim. Tests point this at
/// the built dew-agent; in the guest it is the running agent.
pub static SELF_EXE: std::sync::OnceLock<std::path::PathBuf> = std::sync::OnceLock::new();

fn self_exe() -> std::path::PathBuf {
    SELF_EXE
        .get()
        .cloned()
        .unwrap_or_else(|| "/proc/self/exe".into())
}

/// The re-exec into the confine shim for a confined batch exec (Go:
/// confineExecCmd): `[self, --dew-confine-shim, command, args...]` with the
/// spec in DEW_CONFINE_SPEC and the agent's DEW_EXEC_USER in
/// DEW_CONFINE_EXEC_USER. Both are stripped from the request env first, so
/// a workload can't inject its own spec or identity. The shim runs as root
/// (it needs to for the mount and capability work) and drops privilege
/// itself, so no uid/gid is set here; it resolves `command` on PATH.
pub fn confined_command(req: &ExecRequest, exec_user: Option<&str>) -> Result<Command, String> {
    use crate::confine::{EXEC_USER_ENV, SHIM_MARKER, SPEC_ENV};
    let spec = serde_json::to_string(req.confine.as_ref().ok_or("no confine spec")?)
        .map_err(|e| format!("confine spec: {e}"))?;
    if !req.dir.is_empty() {
        if let Err(e) = std::fs::metadata(&req.dir) {
            return Err(format!("chdir {}: {}", req.dir, gofmt::errno_text(&e)));
        }
    }
    let mut cmd = Command::new(self_exe());
    cmd.arg(SHIM_MARKER).arg(&req.command).args(&req.args);
    if !req.dir.is_empty() {
        cmd.current_dir(&req.dir);
    }
    let base: Vec<String> = std::env::vars().map(|(k, v)| format!("{k}={v}")).collect();
    let control = |e: &String| {
        e.starts_with(&format!("{SPEC_ENV}=")) || e.starts_with(&format!("{EXEC_USER_ENV}="))
    };
    let mut env: Vec<String> = exec_env(&base, &req.env)
        .into_iter()
        .filter(|e| !control(e))
        .collect();
    env.push(format!("{SPEC_ENV}={spec}"));
    if let Some(u) = exec_user.filter(|u| !u.is_empty()) {
        env.push(format!("{EXEC_USER_ENV}={u}"));
    }
    cmd.env_clear().envs(env_pairs(&env));
    cmd.process_group(0);
    Ok(cmd)
}

pub(crate) fn spawn_error(cmd: &Command, e: &io::Error) -> String {
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
/// lands on a live group. `signal_group` gives other callers (the TTY
/// hangup) the same guarantee.
pub(crate) struct Deadline {
    pid: libc::pid_t,
    exited: Mutex<bool>,
    fired: AtomicBool,
}

impl Deadline {
    pub(crate) fn arm(
        child: &Child,
        timeout: Option<Duration>,
    ) -> (Arc<Deadline>, Option<mpsc::Sender<()>>) {
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
                k.signal_locked(libc::SIGKILL, true);
            }
        });
        (d, Some(tx))
    }

    /// Send `sig` to the child's process group unless the child has been
    /// reaped (see the type docs). Returns whether it was sent.
    pub(crate) fn signal_group(&self, sig: libc::c_int) -> bool {
        self.signal_locked(sig, false)
    }

    /// `fired` is set under the same lock as the kill, before `wait` can see
    /// the exit, so a timeout can never be reported as a plain signal.
    fn signal_locked(&self, sig: libc::c_int, mark_fired: bool) -> bool {
        let exited = self.exited.lock().unwrap();
        if *exited {
            return false;
        }
        if mark_fired {
            self.fired.store(true, Ordering::SeqCst);
        }
        unsafe { libc::kill(-self.pid, sig) };
        true
    }

    pub(crate) fn wait(
        &self,
        child: &mut Child,
        cancel: Option<mpsc::Sender<()>>,
    ) -> io::Result<ExitStatus> {
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

/// Bytes of stderr kept at each end of a failed buffered exec, as cmd.Output.
pub const STDERR_WINDOW: usize = 32 << 10;

/// Port of os/exec's prefixSuffixSaver: keeps the first and last `n` bytes
/// written and counts what falls between, so a noisy command can't grow the
/// agent's memory or the reply frame without bound.
pub struct PrefixSuffix {
    n: usize,
    prefix: Vec<u8>,
    suffix: std::collections::VecDeque<u8>,
    skipped: u64,
}

impl PrefixSuffix {
    pub fn new(n: usize) -> Self {
        PrefixSuffix {
            n,
            prefix: Vec::new(),
            suffix: Default::default(),
            skipped: 0,
        }
    }

    pub fn write(&mut self, mut p: &[u8]) {
        let room = self.n - self.prefix.len();
        let take = room.min(p.len());
        self.prefix.extend_from_slice(&p[..take]);
        p = &p[take..];
        // Only the last n bytes of what's left can survive into the suffix.
        if p.len() > self.n {
            self.skipped += (p.len() - self.n) as u64;
            p = &p[p.len() - self.n..];
        }
        self.suffix.extend(p);
        let over = self.suffix.len().saturating_sub(self.n);
        self.suffix.drain(..over);
        self.skipped += over as u64;
    }

    /// The saved bytes, with Go's "... omitting N bytes ..." marker when
    /// anything was dropped.
    pub fn into_bytes(self) -> Vec<u8> {
        let mut out = self.prefix;
        if self.skipped > 0 {
            out.extend_from_slice(
                format!("\n... omitting {} bytes ...\n", self.skipped).as_bytes(),
            );
        }
        out.extend(self.suffix);
        out
    }
}

fn read_bounded(mut r: impl Read + Send + 'static, n: usize) -> thread::JoinHandle<Vec<u8>> {
    thread::spawn(move || {
        let mut saver = PrefixSuffix::new(n);
        let mut buf = [0u8; 16 * 1024];
        loop {
            match r.read(&mut buf) {
                Ok(0) => break,
                Ok(k) => saver.write(&buf[..k]),
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(_) => break,
            }
        }
        saver.into_bytes()
    })
}

/// Run a non-streaming exec and collect its output (executeCommand).
pub fn execute(req: &ExecRequest, cred: Option<Cred>) -> ExecResponse {
    execute_as(req, cred, None)
}

/// execute, also given the agent's DEW_EXEC_USER name, which a confined
/// exec's shim resolves itself (with the unit's User= taking precedence).
/// A confined exec that fails to apply its spec exits 126 with
/// `dew-confine: <reason>` on stderr, as with the Go agent.
pub fn execute_as(req: &ExecRequest, cred: Option<Cred>, exec_user: Option<&str>) -> ExecResponse {
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
    let timeout = timeout_of(req);
    let built = if req.confine.as_ref().is_some_and(|c| c.is_set()) {
        confined_command(req, exec_user)
    } else {
        build(req, cred)
    };
    let mut cmd = match built {
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
    let err = read_bounded(child.stderr.take().unwrap(), STDERR_WINDOW);
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

/// Trailing bytes of a mid-line piece to carry into the next one: a final
/// `\r` may be the first half of a CRLF split across the chunk boundary, and
/// a UTF-8 character cut in half would otherwise decode as two U+FFFD.
fn hold_back(piece: &[u8]) -> usize {
    if piece.last() == Some(&b'\r') {
        return 1;
    }
    incomplete_utf8_tail(piece)
}

/// Length of an unfinished UTF-8 sequence at the end of `p` (0 if none).
/// Looks only at the last three bytes, so invalid bytes earlier in the
/// piece don't hide a character split at its end.
fn incomplete_utf8_tail(p: &[u8]) -> usize {
    for i in 1..=p.len().min(3) {
        let b = p[p.len() - i];
        if b & 0xC0 == 0x80 {
            continue; // continuation byte: keep looking for the lead byte
        }
        let need = match b {
            0xF0.. => 4,
            0xE0.. => 3,
            0xC0.. => 2,
            _ => 1,
        };
        return if need > i { i } else { 0 };
    }
    0
}

/// Send one pipe's output as OutputChunk frames, a line at a time, with the
/// line endings bufio.ScanLines gives the Go agent: "\r\n" and "\n" become
/// "\n", a final unterminated line gets one, and a \r just before EOF is
/// dropped. Lines over MAX_CHUNK go out in pieces; the line state carries
/// across pieces, so these rules hold wherever a chunk boundary falls.
fn pump<C: Conn>(
    pipe: impl Read + Send + 'static,
    stream: &'static str,
    w: Arc<Mutex<C>>,
) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        let send = |data: &[u8]| {
            let chunk = OutputChunk {
                stream: stream.into(),
                data: String::from_utf8_lossy(data).into_owned(),
            };
            write_frame(&mut *w.lock().unwrap(), &chunk).is_ok()
        };
        let mut r = BufReader::with_capacity(MAX_CHUNK as usize, pipe);
        // Bytes of the current piece; may start with bytes held back from the
        // previous piece of the same line.
        let mut buf: Vec<u8> = Vec::with_capacity(256);
        // Part of the current line has already been sent.
        let mut mid_line = false;
        loop {
            let limit = MAX_CHUNK - buf.len() as u64;
            let n = match r.by_ref().take(limit).read_until(b'\n', &mut buf) {
                Ok(n) => n,
                Err(_) => return,
            };
            if n == 0 {
                // EOF: finish a pending line as ScanLines' final token.
                if mid_line || !buf.is_empty() {
                    if buf.last() == Some(&b'\r') {
                        buf.pop();
                    }
                    buf.push(b'\n');
                    send(&buf);
                }
                return;
            }
            if buf.last() == Some(&b'\n') {
                buf.pop();
                if buf.last() == Some(&b'\r') {
                    buf.pop();
                }
                buf.push(b'\n');
                if !send(&buf) {
                    return;
                }
                buf.clear();
                mid_line = false;
                continue;
            }
            // Cut at MAX_CHUNK in the middle of a line.
            let tail = buf.split_off(buf.len() - hold_back(&buf));
            if !buf.is_empty() && !send(&buf) {
                return;
            }
            buf = tail;
            mid_line = true;
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
    if req.confine.as_ref().is_some_and(|c| c.is_set()) {
        return fail(CONFINE_STREAMING);
    }
    if req.tty {
        return done(crate::tty::run(&conn, &writer, req, cred));
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

#[cfg(test)]
mod tests {
    use super::{incomplete_utf8_tail, PrefixSuffix};

    #[test]
    fn incomplete_utf8_tail_finds_only_unfinished_sequences() {
        let e = "é".as_bytes(); // C3 A9
        let g = "😀".as_bytes(); // F0 9F 98 80
        assert_eq!(incomplete_utf8_tail(b""), 0);
        assert_eq!(incomplete_utf8_tail(b"abc"), 0);
        assert_eq!(incomplete_utf8_tail(e), 0, "complete 2-byte char");
        assert_eq!(incomplete_utf8_tail(g), 0, "complete 4-byte char");
        assert_eq!(incomplete_utf8_tail(&e[..1]), 1);
        assert_eq!(incomplete_utf8_tail(&g[..1]), 1);
        assert_eq!(incomplete_utf8_tail(&g[..2]), 2);
        assert_eq!(incomplete_utf8_tail(&g[..3]), 3);
        // An invalid byte earlier must not mask a split character at the end.
        assert_eq!(incomplete_utf8_tail(&[0xFF, b'a', 0xC3]), 1);
        // Stray continuation bytes with no lead in reach: nothing to hold.
        assert_eq!(incomplete_utf8_tail(&[b'a', 0x80, 0x80, 0x80]), 0);
    }

    /// What prefixSuffixSaver.Bytes returns for `data` written in any split.
    fn reference(data: &[u8], n: usize) -> Vec<u8> {
        if data.len() <= 2 * n {
            return data.to_vec();
        }
        let mut out = data[..n].to_vec();
        out.extend_from_slice(
            format!("\n... omitting {} bytes ...\n", data.len() - 2 * n).as_bytes(),
        );
        out.extend_from_slice(&data[data.len() - n..]);
        out
    }

    #[test]
    fn matches_go_for_every_write_split() {
        let n = 8;
        for len in [0, 1, 7, 8, 9, 16, 17, 25, 100] {
            let data: Vec<u8> = (0..len).map(|i| b'a' + (i % 26) as u8).collect();
            for step in [1, 3, 8, 9, 64] {
                let mut s = PrefixSuffix::new(n);
                for chunk in data.chunks(step) {
                    s.write(chunk);
                }
                assert_eq!(s.into_bytes(), reference(&data, n), "len={len} step={step}");
            }
        }
    }
}
