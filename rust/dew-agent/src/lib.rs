//! dew guest agent, Rust port of cmd/dew-agent (see
//! docs/rust-agent-migration.md). Not shipped yet: the initramfs still bakes
//! the Go agent.
//!
//! Implemented: set_token (fail-closed auth), buffered and streaming exec
//! (with stdin), TTY exec on a pty, connect proxy, set_exposes with the
//! reverse dial to the host, ping, DEW_EXEC_USER, DEW_VSOCK_PORT, PATH pin,
//! periodic sync. Not yet: --confine (phase 4); it fails closed with an
//! explicit error, and the set_token ack reports confine: false so the host
//! refuses --confine against this build.

pub mod confine;
pub mod conn;
pub mod env;
pub mod exec;
pub mod expose;
pub mod gofmt;
pub mod pty;
pub mod server;
pub mod tty;
#[cfg(target_os = "linux")]
pub mod vsock;
