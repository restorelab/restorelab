package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Storage is a storage as the cluster sees it, used by diagnostics: knowing
// which storages exist, what they hold and whether they are enabled is the
// first question when a workload appears to have no backups.
type Storage struct {
	ID        string
	Type      string
	Content   string // comma-separated content types, as PVE reports them
	Active    bool
	Enabled   bool
	Shared    bool
	TotalByte int64
	UsedByte  int64
}

// HoldsBackups reports whether this storage advertises backup content.
func (s Storage) HoldsBackups() bool {
	for _, c := range splitCSV(s.Content) {
		if c == "backup" {
			return true
		}
	}
	return false
}

// ListStorages returns every storage visible on a node. Pass an empty node to
// use the first online one.
func (p *Provider) ListStorages(ctx context.Context, node string) ([]Storage, error) {
	if node == "" {
		nodes, err := p.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if n.Online {
				node = n.ID
				break
			}
		}
		if node == "" {
			return nil, fmt.Errorf("proxmox: no online node to list storages on")
		}
	}

	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/storage", node), nil)
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode storage list for node %s: %w", node, err)
	}

	out := make([]Storage, 0, len(entries))
	for _, e := range entries {
		out = append(out, Storage{
			ID:        asString(e["storage"]),
			Type:      asString(e["type"]),
			Content:   asString(e["content"]),
			Active:    asBool(e["active"]),
			Enabled:   asBool(e["enabled"]),
			Shared:    asBool(e["shared"]),
			TotalByte: asInt64(e["total"]),
			UsedByte:  asInt64(e["used"]),
		})
	}
	return out, nil
}

// CountBackups reports how many backup volumes a storage holds, for every
// workload or for one in particular. It answers "is anything actually in
// there?" without the caller having to parse volume ids.
func (p *Provider) CountBackups(ctx context.Context, node, storage, workloadID string) (int, error) {
	params := url.Values{"content": {"backup"}}
	if workloadID != "" {
		params.Set("vmid", workloadID)
	}
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage), params)
	if err != nil {
		return 0, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, fmt.Errorf("proxmox: decode content of storage %s: %w", storage, err)
	}
	return len(entries), nil
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if part := s[start:i]; part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
