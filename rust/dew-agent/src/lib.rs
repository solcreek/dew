//! dew guest agent, Rust port of cmd/dew-agent (migration phase 2, see
//! docs/rust-agent-migration.md). Not shipped yet: the initramfs still bakes
//! the Go agent.

pub mod conn;
pub mod env;
pub mod exec;
pub mod gofmt;
