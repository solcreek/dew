//! dew guest agent: the daemon every initramfs profile bakes in at
//! /usr/local/bin/dew-agent (see docs/guest-agent.md).
//!
//! It serves set_token (fail-closed auth), buffered and streaming exec
//! (with stdin), TTY exec on a pty, the connect proxy, set_exposes with the
//! reverse dial to the host, --confine through the re-exec shim (read-only
//! root, identity and capability drop, no_new_privs, seccomp) and ping, and
//! honours DEW_EXEC_USER and DEW_VSOCK_PORT, pins PATH and syncs
//! periodically. The set_token ack advertises confine on Linux.

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
