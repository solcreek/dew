//! dew guest agent, Rust port of cmd/dew-agent (migration phase 2, see
//! docs/rust-agent-migration.md). Not shipped yet: the initramfs still bakes
//! the Go agent.
//!
//! Implemented: set_token (fail-closed auth), buffered and streaming exec
//! (with stdin), connect proxy, ping, DEW_EXEC_USER, DEW_VSOCK_PORT, PATH
//! pin, periodic sync. Not yet: TTY and set_exposes (phase 3), --confine
//! (phase 4). Those requests fail with an explicit error, and the set_token
//! ack reports confine: false, so the host fails closed on --confine.

pub mod conn;
pub mod env;
pub mod exec;
pub mod gofmt;
pub mod server;
#[cfg(target_os = "linux")]
pub mod vsock;
