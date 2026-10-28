//! Wire types and framing for the dew host <-> guest vsock protocol.
//!
//! This is a field-for-field mirror of the Go package `internal/vsock`
//! (`protocol.go`), which stays the source of truth. Encoding parity is
//! enforced by `tests/golden.rs`, which decodes and re-encodes the frames the
//! Go encoder writes to `internal/vsock/testdata/golden/`.
//!
//! Mapping rules from the Go struct tags:
//! - a field without `omitempty` is always serialized (Go zero values included);
//! - `omitempty` maps to `skip_serializing_if` on the zero value;
//! - every struct decodes missing fields as zero values (`#[serde(default)]`),
//!   as `encoding/json` does;
//! - a Go slice without `omitempty` encodes nil as `null`, so it is an
//!   `Option<Vec<_>>` here (`None` <-> `null`).
//!
//! Known divergence: `encoding/json` matches object keys case-insensitively
//! and escapes `<`, `>`, `&` as `<`-style sequences; serde does neither.
//! Both sides always send the exact lowercase keys, and both decoders accept
//! either escaping, so the frames interoperate.

use serde::{Deserialize, Serialize};
use std::io::{self, Read, Write};

/// Guest-side vsock port the agent listens on for host-initiated requests.
pub const DEFAULT_PORT: u32 = 1024;
/// Host-side vsock port the guest dials for `dew up --expose-host`.
pub const REVERSE_FORWARD_PORT: u32 = 1025;
/// Largest payload either side accepts in one frame.
pub const MAX_FRAME: u32 = 10 * 1024 * 1024;

pub const TYPE_EXEC: &str = "exec";
pub const TYPE_CONNECT: &str = "connect";
pub const TYPE_SET_TOKEN: &str = "set_token";
pub const TYPE_SET_EXPOSES: &str = "set_exposes";
pub const TYPE_REVERSE_DIAL: &str = "reverse_dial";

fn is_false(b: &bool) -> bool {
    !*b
}
fn is_zero_i64(n: &i64) -> bool {
    *n == 0
}
fn is_zero_u16(n: &u16) -> bool {
    *n == 0
}

/// Just the `type` field, read first to dispatch a request frame.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Envelope {
    #[serde(rename = "type")]
    pub kind: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct SetExposesRequest {
    #[serde(rename = "type")]
    pub kind: String,
    pub token: String,
    pub ports: Option<Vec<i64>>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ReverseDialRequest {
    #[serde(rename = "type")]
    pub kind: String,
    pub token: String,
    pub port: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ReverseDialResponse {
    pub ok: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct SetTokenRequest {
    #[serde(rename = "type")]
    pub kind: String,
    pub token: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ExecRequest {
    #[serde(rename = "type")]
    pub kind: String,
    pub token: String,
    pub command: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub dir: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<String>,
    #[serde(skip_serializing_if = "is_zero_i64")]
    pub timeout_ms: i64,
    #[serde(skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub stdin: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub tty: bool,
    #[serde(skip_serializing_if = "is_zero_u16")]
    pub rows: u16,
    #[serde(skip_serializing_if = "is_zero_u16")]
    pub cols: u16,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub confine: Option<Confinement>,
}

/// Privilege/isolation spec for `dew run --confine` (see the Go type for the
/// enforcement status of each field).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Confinement {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub user: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub group: String,
    #[serde(skip_serializing_if = "is_false")]
    pub dynamic_user: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub no_new_privs: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub drop_all_caps: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub keep_caps: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub drop_caps: Vec<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub read_only_root: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub read_write_paths: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub address_families: Vec<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub address_families_deny: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub system_calls: Vec<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub system_calls_deny: bool,
}

impl Confinement {
    /// Whether the spec constrains anything, so the agent can skip the
    /// re-exec shim when it doesn't. Mirrors `(*Confinement).Set`; note the
    /// Go version also ignores `address_families_deny` / `system_calls_deny`
    /// on their own, since a deny flag without names constrains nothing.
    pub fn is_set(&self) -> bool {
        !self.user.is_empty()
            || !self.group.is_empty()
            || self.dynamic_user
            || self.no_new_privs
            || self.drop_all_caps
            || !self.keep_caps.is_empty()
            || !self.drop_caps.is_empty()
            || self.read_only_root
            || !self.read_write_paths.is_empty()
            || !self.address_families.is_empty()
            || !self.system_calls.is_empty()
    }
}

/// Stdin (or a terminal resize when `winch`) from host to guest during a
/// streaming exec. In TTY mode `data` is base64.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct InputChunk {
    #[serde(rename = "type", skip_serializing_if = "String::is_empty")]
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub data: String,
    #[serde(skip_serializing_if = "is_false")]
    pub eof: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub winch: bool,
    #[serde(skip_serializing_if = "is_zero_u16")]
    pub rows: u16,
    #[serde(skip_serializing_if = "is_zero_u16")]
    pub cols: u16,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ExecResponse {
    pub exit_code: i64,
    pub stdout: String,
    pub stderr: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

/// One chunk of a streaming exec; `stream` is `"stdout"` or `"stderr"`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct OutputChunk {
    pub stream: String,
    pub data: String,
}

/// End of a streaming exec.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ExecDone {
    pub exit_code: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ConnectRequest {
    #[serde(rename = "type")]
    pub kind: String,
    pub token: String,
    pub addr: String,
}

/// Reply to connect / set_token / set_exposes. `confine` is the set_token
/// acknowledgement that the agent enforces `ExecRequest::confine` natively; an
/// agent that omits it makes the host fail closed on `--confine`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ConnectResponse {
    pub ok: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(skip_serializing_if = "is_false")]
    pub confine: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct PingResponse {
    pub status: String,
}

/// Encode `v` as one frame: 4-byte big-endian length, then the JSON payload.
/// The frame goes out in a single `write_all` on one buffer so callers that
/// serialize writers with a lock never emit half a frame between others.
pub fn encode_frame(v: &impl Serialize) -> io::Result<Vec<u8>> {
    let mut buf = vec![0u8; 4];
    serde_json::to_writer(&mut buf, v)?;
    let len = buf.len() - 4;
    if len > MAX_FRAME as usize {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("payload too large: {len} bytes"),
        ));
    }
    buf[..4].copy_from_slice(&(len as u32).to_be_bytes());
    Ok(buf)
}

pub fn write_frame(w: &mut impl Write, v: &impl Serialize) -> io::Result<()> {
    w.write_all(&encode_frame(v)?)
}

/// Read one frame's raw payload, rejecting anything over [`MAX_FRAME`].
pub fn read_frame_bytes(r: &mut impl Read) -> io::Result<Vec<u8>> {
    let mut hdr = [0u8; 4];
    r.read_exact(&mut hdr)?;
    let len = u32::from_be_bytes(hdr);
    if len > MAX_FRAME {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("payload too large: {len} bytes"),
        ));
    }
    let mut buf = vec![0u8; len as usize];
    r.read_exact(&mut buf)?;
    Ok(buf)
}

pub fn read_frame<T: for<'de> Deserialize<'de>>(r: &mut impl Read) -> io::Result<T> {
    let buf = read_frame_bytes(r)?;
    serde_json::from_slice(&buf).map_err(io::Error::from)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frame_round_trip() {
        let req = ExecRequest {
            command: "ls".into(),
            args: vec!["-la".into()],
            ..Default::default()
        };
        let frame = encode_frame(&req).unwrap();
        assert_eq!(
            u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize,
            frame.len() - 4
        );
        let got: ExecRequest = read_frame(&mut frame.as_slice()).unwrap();
        assert_eq!(got, req);
    }

    #[test]
    fn one_write_per_frame() {
        struct Counter(Vec<usize>);
        impl Write for Counter {
            fn write(&mut self, b: &[u8]) -> io::Result<usize> {
                self.0.push(b.len());
                Ok(b.len())
            }
            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }
        let mut w = Counter(vec![]);
        write_frame(
            &mut w,
            &OutputChunk {
                stream: "stdout".into(),
                data: "x\n".into(),
            },
        )
        .unwrap();
        assert_eq!(w.0.len(), 1, "frame split across writes: {:?}", w.0);
    }

    #[test]
    fn rejects_oversized_header() {
        let hdr = [0x01u8, 0, 0, 0]; // 16 MiB
        let err = read_frame_bytes(&mut hdr.as_slice()).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
    }

    #[test]
    fn refuses_to_encode_an_oversized_frame() {
        // The peer would reject it anyway; fail at the sender instead.
        let big = OutputChunk {
            stream: "stdout".into(),
            data: "x".repeat(MAX_FRAME as usize),
        };
        let err = encode_frame(&big).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
    }

    #[test]
    fn rejects_truncated_header() {
        assert!(read_frame_bytes(&mut [0u8, 0].as_slice()).is_err());
    }

    #[test]
    fn missing_fields_decode_as_zero_values() {
        let r: ExecResponse = serde_json::from_str("{}").unwrap();
        assert_eq!(r, ExecResponse::default());
        let r: SetExposesRequest = serde_json::from_str(r#"{"ports":null}"#).unwrap();
        assert_eq!(r.ports, None);
    }

    #[test]
    fn unknown_fields_are_ignored() {
        // A newer peer may add fields; encoding/json ignores them, so must we.
        let r: ConnectResponse = serde_json::from_str(r#"{"ok":true,"future":1}"#).unwrap();
        assert!(r.ok);
    }

    #[test]
    fn confinement_is_set() {
        assert!(!Confinement::default().is_set());
        assert!(!Confinement {
            system_calls_deny: true,
            ..Default::default()
        }
        .is_set());
        assert!(Confinement {
            read_only_root: true,
            ..Default::default()
        }
        .is_set());
    }
}
