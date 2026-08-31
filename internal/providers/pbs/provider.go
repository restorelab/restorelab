package pbs

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// compile-time assertion that Provider satisfies core.BackupProvider.
var _ core.BackupProvider = (*Provider)(nil)

// Snapshot is the raw PBS-side coordinates a core.Backup was derived from,
// for callers that need to talk to the PBS API directly (prune, file-level
// restore, verify-on-demand, ...) rather than go through PVE's restore call.
type Snapshot struct {
	BackupType string // PBS "backup-type": vm, ct or host.
	BackupID   string // PBS "backup-id": the VMID/CTID/hostname.
	BackupTime time.Time
}

// SnapshotPath returns the PBS "type/id/time" path used throughout the PBS
// API and CLI, e.g. "vm/101/2026-08-31T03:00:00Z".
func (s Snapshot) SnapshotPath() string {
	return fmt.Sprintf("%s/%s/%s", s.BackupType, s.BackupID, s.BackupTime.UTC().Format(time.RFC3339))
}

// ID returns the user-facing identifier of this provider instance.
func (p *Provider) ID() string { return p.cfg.ID }

// Kind identifies the provider technology.
func (p *Provider) Kind() string { return "pbs" }

// Ping validates that the PBS API is reachable and the configured token is
// accepted.
func (p *Provider) Ping(ctx context.Context) error {
	var v versionResponse
	if err := p.get(ctx, "/api2/json/version", nil, &v); err != nil {
		return fmt.Errorf("pbs: ping: %w", err)
	}
	return nil
}

// ListDatastores lists every datastore this PBS instance exposes. It is a
// discovery helper for the CLI (picking which datastore/PVEStorage pair to
// configure) and is not part of core.BackupProvider.
func (p *Provider) ListDatastores(ctx context.Context) ([]Datastore, error) {
	var entries []datastoreEntry
	if err := p.get(ctx, "/api2/json/admin/datastore", nil, &entries); err != nil {
		return nil, fmt.Errorf("pbs: listing datastores: %w", err)
	}

	out := make([]Datastore, 0, len(entries))
	for _, e := range entries {
		name := e.Store
		if name == "" {
			// Some PBS versions/endpoints report the datastore name under
			// "name" instead of "store"; tolerate both.
			name = e.Name
		}
		out = append(out, Datastore{Name: name, Path: e.Path, Comment: e.Comment})
	}
	return out, nil
}

// ListBackups returns every "vm" snapshot for workloadID in the configured
// datastore, newest first.
func (p *Provider) ListBackups(ctx context.Context, workloadID string) ([]core.Backup, error) {
	query := url.Values{
		"backup-type": {"vm"},
		"backup-id":   {workloadID},
	}
	path := fmt.Sprintf("/api2/json/admin/datastore/%s/snapshots", url.PathEscape(p.cfg.Datastore))

	var entries []snapshotEntry
	if err := p.get(ctx, path, query, &entries); err != nil {
		return nil, fmt.Errorf("pbs: listing backups for workload %s: %w", workloadID, err)
	}

	backups := make([]core.Backup, 0, len(entries))
	for _, e := range entries {
		b, err := p.toBackup(e)
		if err != nil {
			return nil, fmt.Errorf("pbs: listing backups for workload %s: %w", workloadID, err)
		}
		backups = append(backups, b)
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// GetLatestBackup returns the most recent backup for workloadID, or
// core.ErrNoBackup when none exist.
//
// When Config.SkipFailedVerification is set, snapshots whose PBS
// verification job reported "failed" are skipped in favour of an older one;
// otherwise the newest snapshot is returned regardless of verification
// state. See the field doc on Config.SkipFailedVerification for the
// trade-off.
func (p *Provider) GetLatestBackup(ctx context.Context, workloadID string) (*core.Backup, error) {
	backups, err := p.ListBackups(ctx, workloadID)
	if err != nil {
		return nil, err
	}

	for i := range backups {
		if p.cfg.SkipFailedVerification && backups[i].Verified == core.VerificationFailed {
			continue
		}
		return &backups[i], nil
	}

	return nil, core.ErrNoBackup
}

// toBackup maps one raw PBS snapshot listing entry onto core.Backup.
func (p *Provider) toBackup(e snapshotEntry) (core.Backup, error) {
	ts, err := e.BackupTime.Int64()
	if err != nil {
		return core.Backup{}, fmt.Errorf("snapshot %s/%s: backup-time: %w", e.BackupType, e.BackupID, err)
	}
	createdAt := time.Unix(ts, 0).UTC()

	size, err := e.Size.Int64()
	if err != nil {
		return core.Backup{}, fmt.Errorf("snapshot %s/%s: size: %w", e.BackupType, e.BackupID, err)
	}

	return core.Backup{
		ID:         volID(p.cfg.PVEStorage, e.BackupID, createdAt),
		WorkloadID: e.BackupID,
		ProviderID: p.cfg.ID,
		Datastore:  p.cfg.Datastore,
		CreatedAt:  createdAt,
		SizeBytes:  size,
		Protected:  e.Protected,
		Encrypted:  isEncrypted(e.Files),
		Verified:   verificationState(e.Verification),
		// PBS stores backups in its own chunked/pxar format, not a
		// PVE-native disk format, so "pbs" is the meaningful value here
		// rather than e.g. "qcow2"/"vma".
		Format: "pbs",
		Notes:  e.Comment,
	}, nil
}

// volID builds the PVE volid this backup is addressed by when restored
// through Proxmox VE: "<storage>:backup/vm/<vmid>/<RFC3339 UTC timestamp>".
// PBS itself has no notion of a "storage" prefix -- it addresses a snapshot
// by type/id/time alone -- but this exact string is what PVE's own restore
// call expects, since PVEStorage is the name under which PVE has this PBS
// datastore mounted. Constructing it here keeps the PBS<->PVE naming
// convention in one place instead of duplicating it in the PVE package.
func volID(pveStorage, workloadID string, t time.Time) string {
	return fmt.Sprintf("%s:backup/vm/%s/%s", pveStorage, workloadID, t.UTC().Format(time.RFC3339))
}

// verificationState maps a possibly-absent PBS verification object onto
// core.VerificationState. PBS omits "verification" entirely for a snapshot
// that was never verified, which is core.VerificationNone -- distinct from
// core.VerificationUnknown, which we reserve for a state string PBS sends
// that we don't otherwise recognise.
func verificationState(v *snapshotVerification) core.VerificationState {
	if v == nil {
		return core.VerificationNone
	}
	switch v.State {
	case "ok":
		return core.VerificationOK
	case "failed":
		return core.VerificationFailed
	default:
		return core.VerificationUnknown
	}
}

// isEncrypted reports whether any file in the snapshot was encrypted. PBS
// sets crypt-mode per file (typically consistent across the snapshot); a
// missing/"none" crypt-mode means that file is not encrypted.
func isEncrypted(files []snapshotFile) bool {
	for _, f := range files {
		if f.CryptMode != "" && f.CryptMode != "none" {
			return true
		}
	}
	return false
}
