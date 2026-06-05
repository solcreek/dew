//go:build darwin

package darwin

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/solcreek/dew/internal/vm"
)

// dumpConfigSummary writes a structured summary of the VM config that
// will be handed to Apple Virtualization. Emitted to stderr when
// DEW_DEBUG=1 is set, and inlined into `dew doctor --verbose` output.
//
// Apple's VZErrorDomain Code=1 ("Internal Virtualization error") is
// frequently the only signal a user gets when boot fails — the
// underlying reason (wrong kernel format, model-specific incompat,
// device mis-config) is left out of the NSError chain. A pre-boot
// dump of what we actually sent lets bug reports show whether the
// config or the platform is at fault.
func dumpConfigSummary(w io.Writer, cfg vm.Config) {
	fmt.Fprintln(w, "── dew VM config ──")
	host := readHostInfo()
	fmt.Fprintf(w, "  host:     %s arch=%s macOS %s\n",
		strNonEmpty(host.Model, "<unknown>"), runtime.GOARCH, strNonEmpty(host.OSVersion, "<unknown>"))
	fmt.Fprintf(w, "  cpus:     %d\n", cfg.CPUs)
	fmt.Fprintf(w, "  memory:   %d MB (%d bytes)\n", cfg.MemoryMB, cfg.MemoryMB*1024*1024)
	fmt.Fprintf(w, "  cmdline:  %s\n", cfg.CmdLine)

	kHdr := readBinaryHeader(cfg.Kernel)
	fmt.Fprintf(w, "  kernel:   %s\n            size=%d bytes  first4=%02x %02x %02x %02x  arm64-magic@0x38=%02x %02x %02x %02x  %s\n",
		cfg.Kernel, kHdr.Size,
		kHdr.First4[0], kHdr.First4[1], kHdr.First4[2], kHdr.First4[3],
		kHdr.ARM64Magic[0], kHdr.ARM64Magic[1], kHdr.ARM64Magic[2], kHdr.ARM64Magic[3],
		kernelFormatHint(kHdr))

	if cfg.Initrd != "" {
		if st, err := os.Stat(cfg.Initrd); err == nil {
			fmt.Fprintf(w, "  initrd:   %s (%d bytes)\n", cfg.Initrd, st.Size())
		} else {
			fmt.Fprintf(w, "  initrd:   %s (stat error: %v)\n", cfg.Initrd, err)
		}
	}

	fmt.Fprintf(w, "  network:  %v", cfg.Network)
	if cfg.NetworkPolicy != "" {
		fmt.Fprintf(w, " (policy=%s)", cfg.NetworkPolicy)
	}
	fmt.Fprintln(w)

	if cfg.DiskPath != "" {
		fmt.Fprintf(w, "  disk:     %s (%d GB)\n", cfg.DiskPath, cfg.DiskGB)
	}
	if cfg.VsockPort != 0 {
		fmt.Fprintf(w, "  vsock:    port %d\n", cfg.VsockPort)
	}
	if n := len(cfg.SharedDirs); n > 0 {
		fmt.Fprintf(w, "  shared:   %d mount(s)\n", n)
		for _, sd := range cfg.SharedDirs {
			mode := "rw"
			if sd.ReadOnly {
				mode = "ro"
			}
			fmt.Fprintf(w, "            %s  ->  %s (%s)\n", sd.HostPath, sd.Tag, mode)
		}
	}
	if n := len(cfg.Forwards); n > 0 {
		fmt.Fprintf(w, "  forwards: %d port(s)\n", n)
		for _, f := range cfg.Forwards {
			fmt.Fprintf(w, "            host:%d -> guest:%d\n", f.HostPort, f.GuestPort)
		}
	}
	fmt.Fprintln(w, "────────────────────")
}

// kernelFormatHint classifies the kernel file by both the first 4
// bytes and the magic at offset 0x38. Apple VZ on ARM64 requires a
// raw uncompressed Linux Image; that format is canonically identified
// by the `ARM\x64` (0x64644d52 LE → "ARMd") magic at offset 0x38.
//
// The first 4 bytes of a valid ARM64 Linux Image are ALSO "MZ@..." —
// the kernel deliberately ships an EFI stub at offset 0 that doubles
// as a valid ARM64 branch instruction, so the same bytes boot either
// way. Earlier versions of this heuristic flagged any "MZ" as
// "EFI/PE bad", which falsely accused every modern ARM64 Linux
// kernel. The offset-0x38 magic is the authoritative check.
//
// Field reports surfaced the actual broken case: a 9MB EFI-stub-only
// kernel left over from an earlier dew install — same MZ prefix but
// no ARM64 boot header at 0x38, so Apple VZ correctly rejects it.
// This heuristic now distinguishes the two.
func kernelFormatHint(h binaryHeader) string {
	// Authoritative ARM64 Linux Image check — present regardless of
	// any wrapping at offset 0.
	if h.ARM64Magic == [4]byte{'A', 'R', 'M', 0x64} {
		return "format=raw ARM64 Linux Image (boot header magic present at offset 0x38)"
	}
	// No ARM64 boot header. Classify by first 4 bytes — the formats
	// listed here are all things Apple VZ refuses to load.
	switch {
	case h.First4 == [4]byte{0x1f, 0x8b, 0x08, 0x00},
		h.First4 == [4]byte{0x1f, 0x8b, 0x08, 0x08}:
		return "format=gzip without ARM64 header — Apple VZ requires raw Image; will fail with Code=1"
	case h.First4 == [4]byte{0x28, 0xb5, 0x2f, 0xfd}:
		return "format=zstd without ARM64 header — Apple VZ requires raw Image; will fail with Code=1"
	case h.First4[0] == 'M' && h.First4[1] == 'Z':
		return "format=EFI/PE without ARM64 boot header at offset 0x38 — likely a stale or non-Linux kernel; run `dew assets pull --force`"
	case h.First4 == [4]byte{0x7f, 'E', 'L', 'F'}:
		return "format=ELF — Apple VZ wants the raw Image, not the ELF vmlinux"
	default:
		return "format=unknown (no ARM64 boot header at offset 0x38)"
	}
}

type binaryHeader struct {
	Size       int64
	First4     [4]byte
	ARM64Magic [4]byte // bytes at offset 0x38; "ARM\x64" on a valid ARM64 Linux Image
}

func readBinaryHeader(path string) binaryHeader {
	var h binaryHeader
	st, err := os.Stat(path)
	if err != nil {
		return h
	}
	h.Size = st.Size()
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	// Read enough to cover the ARM64 boot header magic at offset 0x38.
	// 64 bytes is the full ARM64 Linux Image header.
	var buf [64]byte
	n, _ := f.Read(buf[:])
	if n >= 4 {
		copy(h.First4[:], buf[0:4])
	}
	if n >= 0x3c {
		copy(h.ARM64Magic[:], buf[0x38:0x3c])
	}
	return h
}

type hostInfo struct {
	Model     string // e.g. Mac16,6
	OSVersion string // e.g. 15.6.1
}

// readHostInfo returns Mac model + macOS version via sysctl + sw_vers.
// Failures are swallowed (empty strings) — the dump is best-effort
// diagnostic, never a reason to block boot.
func readHostInfo() hostInfo {
	var h hostInfo
	if out, err := exec.Command("sysctl", "-n", "hw.model").Output(); err == nil {
		h.Model = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		h.OSVersion = strings.TrimSpace(string(out))
	}
	return h
}

func strNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
