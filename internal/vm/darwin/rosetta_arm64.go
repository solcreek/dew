//go:build darwin && arm64

package darwin

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

// rosettaShareDevice builds a virtiofs directory-sharing device backed by
// Apple's Rosetta-for-Linux translator. The guest mounts it and registers
// the x86_64 ELF magic with binfmt_misc, after which any amd64 binary runs
// transparently under Rosetta. Installs Rosetta on demand if the host
// supports it but hasn't downloaded it yet.
func rosettaShareDevice() (vz.DirectorySharingDeviceConfiguration, error) {
	switch vz.LinuxRosettaDirectoryShareAvailability() {
	case vz.LinuxRosettaAvailabilityNotSupported:
		return nil, fmt.Errorf("Rosetta for Linux is not supported on this host")
	case vz.LinuxRosettaAvailabilityNotInstalled:
		if err := vz.LinuxRosettaDirectoryShareInstallRosetta(); err != nil {
			return nil, fmt.Errorf("install Rosetta: %w", err)
		}
	}

	share, err := vz.NewLinuxRosettaDirectoryShare()
	if err != nil {
		return nil, fmt.Errorf("rosetta share: %w", err)
	}
	fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration(rosettaTag)
	if err != nil {
		return nil, fmt.Errorf("rosetta fs config: %w", err)
	}
	fsConfig.SetDirectoryShare(share)
	return fsConfig, nil
}
