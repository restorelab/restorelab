package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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

// ListContentIDs returns the volume ids a storage holds, optionally filtered
// by content type and workload. It exists for diagnostics: when a backup is
// known to have been taken but does not show up, the useful question is what
// the storage returns without any filter at all.
func (p *Provider) ListContentIDs(ctx context.Context, node, storage, content, workloadID string) ([]string, error) {
	params := url.Values{}
	if content != "" {
		params.Set("content", content)
	}
	if workloadID != "" {
		params.Set("vmid", workloadID)
	}
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage), params)
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode content of storage %s: %w", storage, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, asString(e["volid"]))
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

// Task is a running or recent node task, used by diagnostics: "your backup is
// still running" is a very different answer from "you have no backups".
type Task struct {
	UPID    string
	Type    string
	ID      string
	Status  string
	Running bool
	User    string
}

// RunningTasks returns the tasks currently running on a node.
func (p *Provider) RunningTasks(ctx context.Context, node string) ([]Task, error) {
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/tasks", node), url.Values{
		"running": {"1"},
		"limit":   {"50"},
	})
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode task list for node %s: %w", node, err)
	}

	out := make([]Task, 0, len(entries))
	for _, e := range entries {
		status := asString(e["status"])
		out = append(out, Task{
			UPID:    asString(e["upid"]),
			Type:    asString(e["type"]),
			ID:      asString(e["id"]),
			Status:  status,
			Running: status == "" || status == "running",
			User:    asString(e["user"]),
		})
	}
	return out, nil
}

// Snapshot is a VM snapshot. RestoreLab never restores from one - they live on
// the same storage as the workload and do not survive its loss - but knowing
// they exist is what lets a diagnostic tell someone that what they took is not
// what they think it is.
type Snapshot struct {
	Name        string
	Description string
	Current     bool
}

// ListSnapshots returns a workload's snapshots. PVE includes a synthetic entry
// named "current" for the live state, which is flagged rather than hidden.
func (p *Provider) ListSnapshots(ctx context.Context, workloadID string) ([]Snapshot, error) {
	node, kind, err := p.resolve(ctx, workloadID)
	if err != nil {
		return nil, err
	}
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/snapshot", node, kind, workloadID), nil)
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode snapshots of %s: %w", workloadID, err)
	}

	out := make([]Snapshot, 0, len(entries))
	for _, e := range entries {
		name := asString(e["name"])
		out = append(out, Snapshot{
			Name:        name,
			Description: asString(e["description"]),
			Current:     name == "current",
		})
	}
	return out, nil
}

// RecentBackupTasks returns the most recent backup tasks on a node, newest
// first, whether they succeeded or not.
func (p *Provider) RecentBackupTasks(ctx context.Context, node string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 20
	}
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/tasks", node), url.Values{
		"typefilter": {"vzdump"},
		"limit":      {strconv.Itoa(limit)},
	})
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode backup task list for node %s: %w", node, err)
	}

	out := make([]Task, 0, len(entries))
	for _, e := range entries {
		status := asString(e["status"])
		out = append(out, Task{
			UPID:    asString(e["upid"]),
			Type:    asString(e["type"]),
			ID:      asString(e["id"]),
			Status:  status,
			Running: status == "",
			User:    asString(e["user"]),
		})
	}
	return out, nil
}

// OK reports whether a finished task succeeded.
func (t Task) OK() bool { return t.Status == "OK" }

// EffectivePermissions returns what this token can actually do, as Proxmox
// itself computes it: path -> privilege -> granted. Asking the cluster beats
// reasoning about which ACL should have applied.
func (p *Provider) EffectivePermissions(ctx context.Context, path string) (map[string]map[string]bool, error) {
	params := url.Values{}
	if path != "" {
		params.Set("path", path)
	}
	raw, err := p.get(ctx, "/access/permissions", params)
	if err != nil {
		return nil, err
	}
	var tree map[string]map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("proxmox: decode permissions: %w", err)
	}

	out := make(map[string]map[string]bool, len(tree))
	for path, privs := range tree {
		set := make(map[string]bool, len(privs))
		for priv, v := range privs {
			set[priv] = asBool(v)
		}
		out[path] = set
	}
	return out, nil
}

// Raw performs a read-only GET against the Proxmox API and returns the decoded
// data payload untouched. It exists for diagnostics: when a listing disagrees
// with the web UI, the only way forward is to look at what the API actually
// answered.
func (p *Provider) Raw(ctx context.Context, path string, params url.Values) ([]byte, error) {
	return p.get(ctx, path, params)
}
