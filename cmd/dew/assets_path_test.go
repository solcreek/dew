package main

import (
	"testing"
)

func TestLegacyAssetFilename(t *testing.T) {
	tests := map[string]string{
		"vmlinuz-aarch64":                   "vmlinuz",
		"vmlinuz-x86_64":                    "vmlinuz",
		"initramfs-minimal-aarch64.cpio.gz": "initramfs-minimal.cpio.gz",
		"initramfs-node-x86_64.cpio.gz":     "initramfs-node.cpio.gz",
		"initramfs-standard-aarch64.cpio.gz": "initramfs-standard.cpio.gz",
		// Unknown / unstructured names are passed through verbatim so a
		// future asset name doesn't accidentally collide with a stripped
		// initramfs filename.
		"dew-rootfs-aarch64.tar.gz": "dew-rootfs-aarch64.tar.gz",
	}
	for in, want := range tests {
		if got := legacyAssetFilename(in); got != want {
			t.Errorf("legacyAssetFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssetCachePath_ContentAddressedWhenSHAEmbedded(t *testing.T) {
	// Save + restore the global so other tests aren't affected.
	prev := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prev }()

	ExpectedAssetSHA = map[string]string{
		"vmlinuz-aarch64": "abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
	}
	got := assetCachePath("/data", "vmlinuz-aarch64")
	want := "/data/vmlinuz-aarch64.abc123de"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAssetCachePath_FallsBackToLegacyWhenNoEmbeddedSHA(t *testing.T) {
	prev := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prev }()
	ExpectedAssetSHA = map[string]string{}

	// Kernel falls back to un-suffixed legacy name — preserves dev
	// builds that have an existing ~/.local/share/dew/vmlinuz cache.
	if got := assetCachePath("/data", "vmlinuz-aarch64"); got != "/data/vmlinuz" {
		t.Errorf("kernel legacy: got %q, want /data/vmlinuz", got)
	}
	if got := assetCachePath("/data", "initramfs-minimal-aarch64.cpio.gz"); got != "/data/initramfs-minimal.cpio.gz" {
		t.Errorf("initramfs legacy: got %q, want /data/initramfs-minimal.cpio.gz", got)
	}
}

func TestAssetCachePath_AssetsWithDifferentSHAsDontCollide(t *testing.T) {
	// The core promise of content-addressing: a binary expecting
	// SHA-A and a binary expecting SHA-B cache to different files.
	prev := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prev }()

	ExpectedAssetSHA = map[string]string{"vmlinuz-aarch64": "aaaaaaaa1111111111111111111111111111111111111111111111111111aaaa"}
	pathA := assetCachePath("/data", "vmlinuz-aarch64")

	ExpectedAssetSHA = map[string]string{"vmlinuz-aarch64": "bbbbbbbb2222222222222222222222222222222222222222222222222222bbbb"}
	pathB := assetCachePath("/data", "vmlinuz-aarch64")

	if pathA == pathB {
		t.Errorf("expected different paths for different SHAs, both got %q", pathA)
	}
}
