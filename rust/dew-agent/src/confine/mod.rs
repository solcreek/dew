//! `--confine` enforcement (Rust port of cmd/dew-agent/{confine,caps,seccomp}.go).
//!
//! The plans here are pure (names to numbers, which caps to drop, which
//! identity to take, the BPF programs) so they are tested on any host; the
//! Linux-only syscalls that apply them live in `apply`.

pub mod caps;
pub mod ids;
pub mod seccomp;
#[rustfmt::skip]
pub mod syscalls;
