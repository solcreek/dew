package main

import (
	"path/filepath"
	"runtime"
	"strings"
)

// runtimeArch returns the asset-name arch suffix for the current
// process — "aarch64" on Apple Silicon, "x86_64" on Intel. Release
// asset names use Linux conventions (aarch64/x86_64) rather than Go's
// (arm64/amd64) because the assets ARE Linux artifacts (kernel,
// initramfs).
func runtimeArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	default:
		return "x86_64"
	}
}

// kernelAssetName returns the canonical release asset name for the
// kernel matching the host arch — e.g. "vmlinuz-aarch64".
func kernelAssetName() string {
	return "vmlinuz-" + runtimeArch()
}

// initrdAssetName returns the canonical release asset name for the
// initramfs of the given profile and the host arch — e.g.
// "initramfs-minimal-aarch64.cpio.gz".
func initrdAssetName(profile string) string {
	return "initramfs-" + profile + "-" + runtimeArch() + ".cpio.gz"
}

// assetCachePath returns the on-disk path where a release asset is
// cached. When the binary has an embedded SHA for this asset
// (ExpectedAssetSHA populated by the release pipeline) the path is
// content-addressed: <dataDir>/<assetName>.<sha8>.
//
// Content-addressing buys three properties:
//   - Multiple dew binaries with different SHA expectations coexist
//     in the same dataDir without fighting over a shared filename.
//   - "Stale asset" is structurally impossible: the file's filename
//     literally IS the content hash, so a wrong-bytes file lives at
//     a different path and gets re-fetched on next use.
//   - Downgrade + re-upgrade is free — both versions' assets are
//     already on disk.
//
// Dev / local builds (ExpectedAssetSHA empty) fall back to the
// legacy un-suffixed path so `make build` keeps working without the
// release pipeline. Users upgrading from pre-0.7.33 inherit the
// legacy file on disk; 0.7.33 just downloads a new one alongside it.
// A separate doctor check can later flag the orphan for cleanup.
func assetCachePath(dataDir, assetName string) string {
	sha := ExpectedAssetSHA[assetName]
	if len(sha) >= 8 {
		return filepath.Join(dataDir, assetName+"."+sha[:8])
	}
	return filepath.Join(dataDir, legacyAssetFilename(assetName))
}

// legacyAssetFilename maps an arch-tagged release asset name to the
// un-suffixed filename pre-0.7.33 used for the local cache. Keeps
// `make build` binaries working with their existing data dirs.
//
//	vmlinuz-aarch64                       → vmlinuz
//	vmlinuz-x86_64                        → vmlinuz
//	initramfs-minimal-aarch64.cpio.gz     → initramfs-minimal.cpio.gz
//	initramfs-standard-x86_64.cpio.gz     → initramfs-standard.cpio.gz
func legacyAssetFilename(assetName string) string {
	if assetName == "vmlinuz-aarch64" || assetName == "vmlinuz-x86_64" {
		return "vmlinuz"
	}
	if strings.HasPrefix(assetName, "initramfs-") && strings.HasSuffix(assetName, ".cpio.gz") {
		// initramfs-<profile>-<arch>.cpio.gz → initramfs-<profile>.cpio.gz
		stem := strings.TrimSuffix(assetName, ".cpio.gz")
		parts := strings.Split(stem, "-")
		if len(parts) >= 3 {
			return strings.Join(parts[:len(parts)-1], "-") + ".cpio.gz"
		}
	}
	return assetName
}
