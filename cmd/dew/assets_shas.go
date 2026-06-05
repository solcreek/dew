package main

// ExpectedAssetSHA maps a release asset filename (vmlinuz-aarch64,
// initramfs-minimal-aarch64.cpio.gz, etc.) to its SHA256 hex digest
// as produced by the release pipeline.
//
// Default: empty. dev / local builds (`make build`, `go build`) never
// populate this map; SHA verification in resolveAssets is a no-op
// when a key is absent, falling back to runtime checksums.txt fetch
// for the network path.
//
// Release builds: the release.yml job writes a sibling file
// assets_shas_generated.go (gitignored) with an init() that
// populates this map. The resulting binary verifies its cached
// kernel + initramfs match the bytes published in the SAME release
// tag — stale or corrupt cached files (the 2026-06 M4 Max class of
// bug) are detected before Apple VZ rejects them with Code=1.
var ExpectedAssetSHA = map[string]string{}
