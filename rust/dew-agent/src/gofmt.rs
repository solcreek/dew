//! Go-compatible renderings of values the host shows to users, so error
//! text reads the same whichever agent produced it.

use std::io;
use std::time::Duration;

/// time.Duration.String for millisecond-granularity durations:
/// 100ms, 1.5s, 30s, 1m30s, 1h0m0s.
pub fn duration(d: Duration) -> String {
    let ms = d.as_millis();
    if ms == 0 {
        return "0s".into();
    }
    if ms < 1000 {
        return format!("{ms}ms");
    }
    let (h, rem) = (ms / 3_600_000, ms % 3_600_000);
    let (m, rem) = (rem / 60_000, rem % 60_000);
    let (s, frac) = (rem / 1000, rem % 1000);
    let mut secs = s.to_string();
    if frac > 0 {
        let f = format!("{frac:03}");
        secs.push('.');
        secs.push_str(f.trim_end_matches('0'));
    }
    match (h, m) {
        (0, 0) => format!("{secs}s"),
        (0, m) => format!("{m}m{secs}s"),
        (h, m) => format!("{h}h{m}m{secs}s"),
    }
}

/// The errno text Go's syscall.Errno prints: libc's message, lowercased
/// first letter ("no such file or directory").
pub fn errno_text(e: &io::Error) -> String {
    let Some(code) = e.raw_os_error() else {
        return e.to_string();
    };
    let msg = unsafe { std::ffi::CStr::from_ptr(libc::strerror(code)) }
        .to_string_lossy()
        .into_owned();
    let mut chars = msg.chars();
    match chars.next() {
        Some(c) => c.to_lowercase().chain(chars).collect(),
        None => msg,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durations_match_go() {
        let ms = Duration::from_millis;
        assert_eq!(duration(ms(0)), "0s");
        assert_eq!(duration(ms(100)), "100ms");
        assert_eq!(duration(ms(1500)), "1.5s");
        assert_eq!(duration(ms(1250)), "1.25s");
        assert_eq!(duration(ms(30_000)), "30s");
        assert_eq!(duration(ms(90_000)), "1m30s");
        assert_eq!(duration(ms(3_600_000)), "1h0m0s");
        assert_eq!(duration(ms(90_061_500)), "25h1m1.5s");
    }

    #[test]
    fn errno_text_is_lowercase() {
        let e = io::Error::from_raw_os_error(libc::ENOENT);
        assert_eq!(errno_text(&e), "no such file or directory");
    }

    #[test]
    fn errno_text_passes_through_non_os_errors() {
        let e = io::Error::other("custom failure");
        assert_eq!(errno_text(&e), "custom failure");
    }
}
