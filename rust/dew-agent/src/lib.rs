//! dew guest agent, Rust port of cmd/dew-agent (see
//! docs/rust-agent-migration.md). Not shipped yet: the initramfs still bakes
//! the Go agent.
//!
//! Implemented: set_token (fail-closed auth), buffered and streaming exec
//! (with stdin), TTY exec on a pty, connect proxy, set_exposes with the
//! reverse dial to the host, --confine through the re-exec shim (read-only
//! root, identity and capability drop, no_new_privs, seccomp), ping,
//! DEW_EXEC_USER, DEW_VSOCK_PORT, PATH pin, periodic sync. That is the Go
//! agent's full feature set; the set_token ack advertises confine on Linux.

pub mod confine;
pub mod conn;
pub mod env;
pub mod exec;
pub mod expose;
pub mod gofmt;
pub mod passwd;
pub mod pty;
pub mod server;
pub mod tty;
#[cfg(target_os = "linux")]
pub mod vsock;
