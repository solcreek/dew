//! dew guest agent: the daemon every initramfs profile bakes in at
//! /usr/local/bin/dew-agent. A port of the retired Go agent (see
//! docs/rust-agent-migration.md).
//!
//! Implemented: set_token (fail-closed auth), buffered and streaming exec
//! (with stdin), TTY exec on a pty, connect proxy, set_exposes with the
//! reverse dial to the host, --confine through the re-exec shim (read-only
//! root, identity and capability drop, no_new_privs, seccomp), ping,
//! DEW_EXEC_USER, DEW_VSOCK_PORT, PATH pin, periodic sync: the Go agent's
//! full feature set. The set_token ack advertises confine on Linux.

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
