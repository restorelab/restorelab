package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// backupNode picks the node whose PVE API is used to list backups for a
// workload. When the workload still exists it uses the node currently
// hosting it (backup storage content is queried through a node path even
// for shared/cluster-wide storage such as PBS). When it doesn't (the source
// was deleted, or this is a disaster-recovery scenario where the source
// cluster node is gone), it falls back to any online node.
func (p *Provider) backupNode(ctx context.Context, workloadID string) (string, error) {
	if node, _, err := p.resolve(ctx, workloadID); err == nil {
		return node, nil
	}
	nodes, err := p.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Online {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("proxmox: no online node available to look up backups for %q: %w", workloadID, core.ErrNotFound)
}

// ListBackups returns every backup of workloadID, newest first. It queries
// cfg.BackupStorage when set, otherwise every storage on the resolved node
// that advertises "backup" content.
func (p *Provider) ListBackups(ctx context.Context, workloadID string) ([]core.Backup, error) {
	node, err := p.backupNode(ctx, workloadID)
	if err != nil {
		return nil, err
	}

	storages, err := p.backupStorages(ctx, node)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var backups []core.Backup
	for _, storage := range storages {
		raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage), url.Values{
			"content": {"backup"},
			"vmid":    {workloadID},
		})
		if err != nil {
			return nil, err
		}
		var entries []map[string]any
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("proxmox: decode content list for storage %s: %w", storage, err)
		}
		for _, e := range entries {
			volid := asString(e["volid"])
			if volid == "" || seen[volid] {
				continue
			}
			seen[volid] = true
			backups = append(backups, core.Backup{
				ID:         volid,
				WorkloadID: workloadID,
				ProviderID: p.ID(),
				Datastore:  storage,
				Node:       node,
				CreatedAt:  time.Unix(asInt64(e["ctime"]), 0),
				SizeBytes:  asInt64(e["size"]),
				Protected:  asBool(e["protected"]),
				Verified:   mapVerification(e["verification"]),
				Format:     asString(e["format"]),
				Notes:      asString(e["notes"]),
			})
		}
	}

	sort.Slice(backups, func(i, j int) bool { return backups[i].CreatedAt.After(backups[j].CreatedAt) })
	return backups, nil
}

// backupStorages resolves the set of storage IDs to query for backups.
func (p *Provider) backupStorages(ctx context.Context, node string) ([]string, error) {
	if p.cfg.BackupStorage != "" {
		return []string{p.cfg.BackupStorage}, nil
	}
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/storage", node), url.Values{"content": {"backup"}})
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode storage list for node %s: %w", node, err)
	}
	storages := make([]string, 0, len(entries))
	for _, e := range entries {
		storages = append(storages, asString(e["storage"]))
	}
	return storages, nil
}

// mapVerification normalises PVE's backup verification field, which arrives
// either as a nested object ({"state":"ok",...}) or, on some PVE/storage
// plugin versions, as that same object pre-encoded into a JSON string.
func mapVerification(v any) core.VerificationState {
	switch t := v.(type) {
	case map[string]any:
		switch asString(t["state"]) {
		case "ok":
			return core.VerificationOK
		case "failed":
			return core.VerificationFailed
		case "":
			return core.VerificationNone
		default:
			return core.VerificationUnknown
		}
	case string:
		if t == "" {
			return core.VerificationNone
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err == nil {
			return mapVerification(m)
		}
		return core.VerificationUnknown
	default:
		return core.VerificationNone
	}
}

// GetLatestBackup returns the most recent backup for a workload.
func (p *Provider) GetLatestBackup(ctx context.Context, workloadID string) (*core.Backup, error) {
	backups, err := p.ListBackups(ctx, workloadID)
	if err != nil {
		return nil, err
	}
	if len(backups) == 0 {
		return nil, fmt.Errorf("proxmox: workload %q: %w", workloadID, core.ErrNoBackup)
	}
	b := backups[0]
	return &b, nil
}
