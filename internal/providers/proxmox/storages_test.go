package proxmox

import (
	"context"
	"testing"
)

func TestListStoragesMapsPVEFields(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/storage", 200, []map[string]any{
		{"storage": "local", "type": "dir", "content": "backup,iso,vztmpl", "active": 1, "enabled": 1, "shared": 0, "total": 1000, "used": 400},
		{"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir", "active": 1, "enabled": 1},
		{"storage": "pbs-main", "type": "pbs", "content": "backup", "active": 0, "enabled": 1, "shared": 1},
	})
	p := newTestProvider(t, m, nil)

	storages, err := p.ListStorages(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListStorages: %v", err)
	}
	if len(storages) != 3 {
		t.Fatalf("got %d storages, want 3", len(storages))
	}

	local := storages[0]
	if local.ID != "local" || local.Type != "dir" {
		t.Errorf("local = %+v", local)
	}
	if !local.Active || !local.Enabled || local.Shared {
		t.Errorf("PVE reports these as 0/1, not booleans: %+v", local)
	}
	if local.TotalByte != 1000 || local.UsedByte != 400 {
		t.Errorf("sizes = %d/%d", local.UsedByte, local.TotalByte)
	}

	if !storages[0].HoldsBackups() {
		t.Error("a storage with content backup,iso,vztmpl holds backups")
	}
	if storages[1].HoldsBackups() {
		t.Error("images,rootdir does not hold backups")
	}
	if !storages[2].HoldsBackups() {
		t.Error("content backup holds backups")
	}
	if storages[2].Active {
		t.Error("an inactive storage must not be reported active")
	}
}

func TestListStoragesFallsBackToAnOnlineNode(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes", 200, []map[string]any{
		{"node": "pve-down", "status": "offline"},
		{"node": "pve2", "status": "online"},
	})
	m.on("GET", "/api2/json/nodes/pve2/storage", 200, []map[string]any{
		{"storage": "local", "type": "dir", "content": "backup"},
	})
	p := newTestProvider(t, m, nil)

	storages, err := p.ListStorages(context.Background(), "")
	if err != nil {
		t.Fatalf("ListStorages: %v", err)
	}
	if len(storages) != 1 || storages[0].ID != "local" {
		t.Errorf("storages = %+v", storages)
	}
}

func TestCountBackups(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/storage/local/content", 200, []map[string]any{
		{"volid": "local:backup/vzdump-qemu-103-2026_08_31-03_00_00.vma.zst"},
		{"volid": "local:backup/vzdump-qemu-103-2026_08_30-03_00_00.vma.zst"},
	})
	p := newTestProvider(t, m, nil)

	n, err := p.CountBackups(context.Background(), "pve1", "local", "103")
	if err != nil {
		t.Fatalf("CountBackups: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}

	rec := m.recorded()[0]
	if got := rec.Query.Get("vmid"); got != "103" {
		t.Errorf("vmid = %q, want 103", got)
	}
	if got := rec.Query.Get("content"); got != "backup" {
		t.Errorf("content = %q, want backup", got)
	}
}

func TestCountBackupsWithoutAWorkloadCountsEverything(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/storage/local/content", 200, []map[string]any{})
	p := newTestProvider(t, m, nil)

	if _, err := p.CountBackups(context.Background(), "pve1", "local", ""); err != nil {
		t.Fatalf("CountBackups: %v", err)
	}
	if m.recorded()[0].Query.Has("vmid") {
		t.Error("no vmid filter must be sent when counting every backup")
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{in: "", want: 0},
		{in: "backup", want: 1},
		{in: "backup,iso,vztmpl", want: 3},
		{in: "backup,,iso", want: 2},
		{in: ",backup,", want: 1},
	}
	for _, tt := range tests {
		if got := len(splitCSV(tt.in)); got != tt.want {
			t.Errorf("splitCSV(%q) = %d parts, want %d", tt.in, got, tt.want)
		}
	}
}
