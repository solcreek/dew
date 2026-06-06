//go:build darwin

package darwin

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

func TestKernelFormatHint(t *testing.T) {
	armMagic := [4]byte{'A', 'R', 'M', 0x64}
	noMagic := [4]byte{}

	tests := []struct {
		name    string
		first4  [4]byte
		atMagic [4]byte
		wantSub string
	}{
		// Real ARM64 Linux kernels: MZ at offset 0 (EFI stub doubling
		// as a valid ARM64 branch) AND ARM\x64 at offset 0x38. The
		// authoritative check is the latter; MZ alone must not damn
		// a kernel that's actually loadable. Regression guard for the
		// 0.7.31 false-positive that misled the M4 Max bug reporter.
		{"valid arm64 image with mz stub", [4]byte{'M', 'Z', 0x40, 0xfa}, armMagic, "raw ARM64 Linux Image"},
		{"valid arm64 image with branch first", [4]byte{0x00, 0x00, 0x00, 0x14}, armMagic, "raw ARM64 Linux Image"},

		// Actually broken formats — no ARM64 boot header at 0x38.
		{"gzip default level", [4]byte{0x1f, 0x8b, 0x08, 0x00}, noMagic, "gzip"},
		{"gzip with filename", [4]byte{0x1f, 0x8b, 0x08, 0x08}, noMagic, "gzip"},
		{"zstd", [4]byte{0x28, 0xb5, 0x2f, 0xfd}, noMagic, "zstd"},
		// EFI-stub-only kernel (the 9MB stale asset the M4 Max user
		// hit): MZ prefix but no ARM\x64 magic.
		{"efi stub without arm64 header", [4]byte{'M', 'Z', 0x90, 0x00}, noMagic, "EFI/PE without ARM64 boot header"},
		{"ELF vmlinux", [4]byte{0x7f, 'E', 'L', 'F'}, noMagic, "ELF"},
		{"unknown bytes", [4]byte{0xde, 0xad, 0xbe, 0xef}, noMagic, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := KernelFormatHint(BinaryHeader{First4: tc.first4, ARM64Magic: tc.atMagic})
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("hint = %q, want substring %q", got, tc.wantSub)
			}
		})
	}
}

func TestDumpConfigSummary_CoversAllFields(t *testing.T) {
	tmp := t.TempDir()
	kernelPath := filepath.Join(tmp, "vmlinuz")
	// Bad kernel: gzip prefix, no ARM64 magic at offset 0x38. Padding
	// to 0x40 so ReadBinaryHeader can read the offset-0x38 region
	// (otherwise ARM64Magic stays zero, which is the same outcome).
	kernelBytes := make([]byte, 0x40)
	copy(kernelBytes, []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xff})
	if err := os.WriteFile(kernelPath, kernelBytes, 0644); err != nil {
		t.Fatal(err)
	}
	initrdPath := filepath.Join(tmp, "initrd.cpio.gz")
	if err := os.WriteFile(initrdPath, bytes.Repeat([]byte{0xaa}, 128), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := vm.Config{
		CPUs:      2,
		MemoryMB:  512,
		Kernel:    kernelPath,
		Initrd:    initrdPath,
		CmdLine:   "console=hvc0 quiet",
		Network:   true,
		VsockPort: 9000,
		SharedDirs: []vm.SharedDir{
			{Tag: "app", HostPath: "/Users/x/proj", ReadOnly: false},
		},
		Forwards: []vm.PortForward{
			{HostPort: 5173, GuestPort: 5173},
		},
	}

	var buf bytes.Buffer
	dumpConfigSummary(&buf, cfg)
	out := buf.String()

	mustContain := []string{
		"cpus:     2",
		"memory:   512 MB",
		"console=hvc0 quiet",
		kernelPath,
		initrdPath,
		"network:  true",
		"vsock:    port 9000",
		"/Users/x/proj  ->  app (rw)",
		"host:5173 -> guest:5173",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n----\n%s", want, out)
		}
	}

	// Kernel format hint: arch-gated by design (the offset-0x38 magic
	// check is meaningful only on arm64). On arm64 hosts we expect the
	// "gzip" classification (since the test kernel has a gzip prefix);
	// on amd64 we expect the explicit "x86_64 build" passthrough.
	var wantKernelHint string
	if runtime.GOARCH == "arm64" {
		wantKernelHint = "gzip"
	} else {
		wantKernelHint = "x86_64 build"
	}
	if !strings.Contains(out, wantKernelHint) {
		t.Errorf("kernel format hint missing %q for host arch %q\n----\n%s",
			wantKernelHint, runtime.GOARCH, out)
	}
}

func TestReadBinaryHeader_ARM64Magic(t *testing.T) {
	// Synthesize the ARM64 Linux Image header layout: MZ at offset 0,
	// ARM\x64 at offset 0x38. Matches the real kernel asset dew ships
	// for arm64. Confirms the offset-0x38 read works on a file that's
	// the minimum size for the header.
	dir := t.TempDir()
	path := filepath.Join(dir, "vmlinuz")
	buf := make([]byte, 0x40)
	buf[0], buf[1], buf[2], buf[3] = 'M', 'Z', 0x40, 0xfa
	buf[0x38], buf[0x39], buf[0x3a], buf[0x3b] = 'A', 'R', 'M', 0x64
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatal(err)
	}
	h := ReadBinaryHeader(path)
	if h.First4 != [4]byte{'M', 'Z', 0x40, 0xfa} {
		t.Errorf("First4 = %v", h.First4)
	}
	if h.ARM64Magic != [4]byte{'A', 'R', 'M', 0x64} {
		t.Errorf("ARM64Magic = %v, want ARM\\x64", h.ARM64Magic)
	}
	got := KernelFormatHint(h)
	if !strings.Contains(got, "raw ARM64 Linux Image") {
		t.Errorf("hint = %q, want raw ARM64 classification", got)
	}
}

func TestReadBinaryHeader_MissingFile(t *testing.T) {
	// Best-effort: missing file returns zero header, never panics.
	h := ReadBinaryHeader("/nonexistent/path/vmlinuz")
	if h.Size != 0 || h.First4 != [4]byte{} {
		t.Errorf("missing file should yield zero header, got %+v", h)
	}
}
