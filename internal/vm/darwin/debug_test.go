//go:build darwin

package darwin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

func TestKernelFormatHint(t *testing.T) {
	tests := []struct {
		name    string
		first4  [4]byte
		wantSub string // substring the hint must contain; "unknown" if blank
	}{
		{"gzip default level", [4]byte{0x1f, 0x8b, 0x08, 0x00}, "gzip"},
		{"gzip with filename", [4]byte{0x1f, 0x8b, 0x08, 0x08}, "gzip"},
		{"zstd", [4]byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd"},
		{"EFI/PE", [4]byte{'M', 'Z', 0x90, 0x00}, "EFI/PE"},
		{"ELF", [4]byte{0x7f, 'E', 'L', 'F'}, "ELF"},
		// ARM64 Image first instruction is typically a long branch like
		// 0x14000000 + offset — not detectable as "good", just "not bad".
		{"raw arm64 image-ish", [4]byte{0x00, 0x00, 0x00, 0x14}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kernelFormatHint(tc.first4)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("hint = %q, want substring %q", got, tc.wantSub)
			}
		})
	}
}

func TestDumpConfigSummary_CoversAllFields(t *testing.T) {
	tmp := t.TempDir()
	kernelPath := filepath.Join(tmp, "vmlinuz")
	if err := os.WriteFile(kernelPath, []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xff}, 0644); err != nil {
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
		"gzip", // kernel format hint fires
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
}

func TestReadBinaryHeader_MissingFile(t *testing.T) {
	// Best-effort: missing file returns zero header, never panics.
	h := readBinaryHeader("/nonexistent/path/vmlinuz")
	if h.Size != 0 || h.First4 != [4]byte{} {
		t.Errorf("missing file should yield zero header, got %+v", h)
	}
}
