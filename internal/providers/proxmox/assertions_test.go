package proxmox

import "github.com/restorelab/restorelab/internal/core"

// Compile-time proof that Provider satisfies every contract it claims to.
var (
	_ core.HypervisorProvider = (*Provider)(nil)
	_ core.BackupProvider     = (*Provider)(nil)
	_ core.NetworkValidator   = (*Provider)(nil)
	_ core.CapacityReporter   = (*Provider)(nil)
)
