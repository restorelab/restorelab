package diag

import (
	"context"
	"fmt"

	"github.com/restorelab/restorelab/internal/providers/proxmox"
)

// storageInspector is what a provider must offer for the storage findings to
// exist. Only Proxmox implements it today.
//
// The type assertion lives in this one file on purpose: it is the only
// provider-specific part of the diagnostic, so adding a second hypervisor
// means writing a second file rather than unpicking Run.
type storageInspector interface {
	ListStorages(ctx context.Context, node string) ([]proxmox.Storage, error)
	CountBackups(ctx context.Context, node, storage, workloadID string) (int, error)
}

// appendStorages reports whether there is anything to restore from.
//
// The finding an operator actually needs is the last one: "no backups found
// on any storage". Everything before it exists to say why.
func (r *Report) appendStorages(ctx context.Context, in Input, node string) {
	pve, ok := in.Provider.(storageInspector)
	if !ok {
		return
	}

	storages, err := pve.ListStorages(ctx, node)
	if err != nil {
		r.fail(AreaStorage, "cannot list storages", err.Error())
		return
	}

	var backupStorages []proxmox.Storage
	for _, s := range storages {
		if s.HoldsBackups() {
			backupStorages = append(backupStorages, s)
		}
	}
	if len(backupStorages) == 0 {
		r.fail(AreaStorage, "no storage on this cluster advertises backup content",
			"RestoreLab restores from Proxmox backups: configure a backup job, or attach a Proxmox Backup Server")
		return
	}

	total := 0
	for _, s := range backupStorages {
		count, err := pve.CountBackups(ctx, node, s.ID, in.WorkloadID)
		switch {
		case err != nil:
			// A read-only token that cannot list a dir storage's content is
			// the single most common misconfiguration - see
			// docs/proxmox-permissions.md on Datastore.AllocateSpace.
			r.warn(AreaStorage, fmt.Sprintf("storage %q (%s): cannot read contents", s.ID, s.Type),
				err.Error())
		case !s.Active:
			r.warn(AreaStorage, fmt.Sprintf("storage %q (%s) is not active", s.ID, s.Type), "")
		default:
			total += count
			scope := "backup(s)"
			if in.WorkloadID != "" {
				scope = fmt.Sprintf("backup(s) for workload %s", in.WorkloadID)
			}
			r.ok(AreaStorage, fmt.Sprintf("storage %q (%s): %d %s", s.ID, s.Type, count, scope), "")
		}
	}
	if total == 0 {
		r.fail(AreaStorage, "no backups found on any storage",
			"there is nothing to recovery-test yet; check that the backup job has finished")
	}
}
