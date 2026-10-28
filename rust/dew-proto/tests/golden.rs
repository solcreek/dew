//! Cross-language parity: every frame the Go encoder wrote to
//! internal/vsock/testdata/golden/ must decode into its mirror type and
//! re-encode to the same bytes. Regenerate the files from Go with
//! `go test ./internal/vsock -run Golden -update`.

use dew_proto::*;
use serde::{de::DeserializeOwned, Serialize};
use std::fs;
use std::path::PathBuf;

fn golden_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../internal/vsock/testdata/golden")
}

fn round_trip<T: Serialize + DeserializeOwned>(name: &str, want: &[u8]) -> Result<(), String> {
    let v: T = serde_json::from_slice(want).map_err(|e| format!("{name}: decode: {e}"))?;
    let got = serde_json::to_vec(&v).map_err(|e| format!("{name}: encode: {e}"))?;
    if got != want {
        return Err(format!(
            "{name}: re-encoding differs from Go\n  go:   {}\n  rust: {}",
            String::from_utf8_lossy(want),
            String::from_utf8_lossy(&got)
        ));
    }
    Ok(())
}

/// Map a golden file name to its Rust type. A name missing here fails the
/// test, so a message added on the Go side can't go unmirrored.
fn check(name: &str, bytes: &[u8]) -> Result<(), String> {
    match name {
        "set_token" => round_trip::<SetTokenRequest>(name, bytes),
        "set_exposes" | "set_exposes_nil_ports" => round_trip::<SetExposesRequest>(name, bytes),
        "reverse_dial_request" => round_trip::<ReverseDialRequest>(name, bytes),
        n if n.starts_with("reverse_dial_response") => {
            round_trip::<ReverseDialResponse>(name, bytes)
        }
        n if n.starts_with("exec_response") => round_trip::<ExecResponse>(name, bytes),
        n if n.starts_with("exec_done") => round_trip::<ExecDone>(name, bytes),
        n if n.starts_with("exec_") => round_trip::<ExecRequest>(name, bytes),
        n if n.starts_with("input_") => round_trip::<InputChunk>(name, bytes),
        "output_chunk" => round_trip::<OutputChunk>(name, bytes),
        "connect_request" => round_trip::<ConnectRequest>(name, bytes),
        n if n.starts_with("connect_response") || n == "set_token_ack_confine" => {
            round_trip::<ConnectResponse>(name, bytes)
        }
        "ping_response" => round_trip::<PingResponse>(name, bytes),
        _ => Err(format!("{name}: no Rust type mapped for this golden frame")),
    }
}

#[test]
fn go_golden_frames_round_trip() {
    let mut entries: Vec<_> = fs::read_dir(golden_dir())
        .expect("golden dir (run `go test ./internal/vsock -run Golden -update`)")
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    entries.sort();
    assert!(
        entries.len() >= 20,
        "expected the full golden set, found {}",
        entries.len()
    );

    let failures: Vec<String> = entries
        .iter()
        .filter_map(|p| {
            let name = p.file_stem().unwrap().to_string_lossy().into_owned();
            let raw = fs::read(p).unwrap();
            let bytes = raw.strip_suffix(b"\n").unwrap_or(&raw);
            check(&name, bytes).err()
        })
        .collect();
    assert!(failures.is_empty(), "{}", failures.join("\n"));
}

#[test]
fn golden_frames_survive_framing() {
    // The same payloads, wrapped in a length-prefixed frame and read back.
    let raw = fs::read(golden_dir().join("exec_confine.json")).unwrap();
    let want: ExecRequest = serde_json::from_slice(&raw).unwrap();
    let frame = encode_frame(&want).unwrap();
    let got: ExecRequest = read_frame(&mut frame.as_slice()).unwrap();
    assert_eq!(got, want);
    assert!(got.confine.as_ref().is_some_and(Confinement::is_set));
}
