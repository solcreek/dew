//go:build darwin && !arm64

package darwin

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

// rosettaShareDevice is unavailable on Intel Macs — Rosetta-for-Linux
// translation only exists on Apple Silicon.
func rosettaShareDevice() (vz.DirectorySharingDeviceConfiguration, error) {
	return nil, fmt.Errorf("Rosetta is only available on Apple Silicon")
}
