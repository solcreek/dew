//! `--confine` enforcement.
//!
//! The plans here are pure (names to numbers, which caps to drop, which
//! identity to take, the BPF programs) so they are tested on any host; the
//! Linux-only syscalls that apply them live in `apply`, run by the re-exec
//! `shim`.

#[cfg(target_os = "linux")]
pub mod apply;
pub mod caps;
pub mod ids;
pub mod seccomp;
#[cfg(target_os = "linux")]
pub mod shim;
#[rustfmt::skip]
pub mod syscalls;

/// argv[1] that turns an agent re-exec into the confine shim.
pub const SHIM_MARKER: &str = "--dew-confine-shim";
/// The Confinement, as JSON, for the shim.
pub const SPEC_ENV: &str = "DEW_CONFINE_SPEC";
/// The agent's own DEW_EXEC_USER, set by the parent. Not DEW_EXEC_USER
/// itself: in the merged environment a
/// request's env could override the agent's value (and, say, turn an
/// unprivileged-exec agent's confined command back into root). A dedicated
/// variable that the parent strips from request env keeps it authoritative.
pub const EXEC_USER_ENV: &str = "DEW_CONFINE_EXEC_USER";
