package proxmox

import (
	"context"
	"errors"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

func TestListBackupsMappingAndSorting(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "node": "pve1"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage", 200, []map[string]any{
		{"storage": "local"},
		{"storage": "pbs-main"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage/local/content", 200, []map[string]any{
		{
			"volid":     "local:backup/vzdump-qemu-101-2026_08_30-03_00_00.vma.zst",
			"ctime":     1000,
			"size":      123456,
			"protected": 0,
			"format":    "vma.zst",
			"notes":     "nightly",
		},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage/pbs-main/content", 200, []map[string]any{
		{
			"volid":        "pbs-main:backup/vm/101/2026-08-31T03:00:00Z",
			"ctime":        2000,
			"size":         654321,
			"protected":    1,
			"format":       "pbs",
			"verification": map[string]any{"state": "ok"},
		},
	})
	p := newTestProvider(t, m, nil)

	backups, err := p.ListBackups(context.Background(), "101")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups, got %d", len(backups))
	}

	// Newest (ctime=2000, pbs-main) first.
	newest := backups[0]
	if newest.ID != "pbs-main:backup/vm/101/2026-08-31T03:00:00Z" {
		t.Errorf("newest.ID = %q", newest.ID)
	}
	if !newest.Protected {
		t.Error("expected newest.Protected = true")
	}
	if newest.Verified != core.VerificationOK {
		t.Errorf("newest.Verified = %v, want VerificationOK", newest.Verified)
	}
	if newest.Datastore != "pbs-main" || newest.Node != "pve1" || newest.WorkloadID != "101" {
		t.Errorf("newest mismapped: %+v", newest)
	}

	oldest := backups[1]
	if oldest.Protected {
		t.Error("expected oldest.Protected = false")
	}
	if oldest.Verified != core.VerificationNone {
		t.Errorf("oldest.Verified = %v, want VerificationNone", oldest.Verified)
	}
	if !oldest.CreatedAt.Before(newest.CreatedAt) {
		t.Errorf("expected oldest.CreatedAt < newest.CreatedAt")
	}
}

func TestListBackupsHonoursConfiguredStorage(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "node": "pve1"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage/pbs-main/content", 200, []map[string]any{
		{"volid": "pbs-main:backup/vm/101/x", "ctime": 1, "size": 1},
	})
	p := newTestProvider(t, m, func(c *Config) { c.BackupStorage = "pbs-main" })

	backups, err := p.ListBackups(context.Background(), "101")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}

	for _, r := range m.recorded() {
		if r.Path == "/api2/json/nodes/pve1/storage" {
			t.Error("BackupStorage is set: must not enumerate storages")
		}
	}
}

func TestGetLatestBackupReturnsErrNoBackup(t *testing.T) {
	m := newMockServer(t)
	// Workload no longer exists: resolve() falls back to any online node.
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{})
	m.on("GET", "/api2/json/nodes", 200, []map[string]any{
		{"node": "pve1", "status": "online"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage", 200, []map[string]any{
		{"storage": "local"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage/local/content", 200, []map[string]any{})
	p := newTestProvider(t, m, nil)

	_, err := p.GetLatestBackup(context.Background(), "999")
	if !errors.Is(err, core.ErrNoBackup) {
		t.Fatalf("expected core.ErrNoBackup, got %v", err)
	}
}

func TestGetLatestBackupReturnsNewest(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "node": "pve1"},
	})
	m.on("GET", "/api2/json/nodes/pve1/storage", 200, []map[string]any{{"storage": "local"}})
	m.on("GET", "/api2/json/nodes/pve1/storage/local/content", 200, []map[string]any{
		{"volid": "local:backup/old", "ctime": 100},
		{"volid": "local:backup/new", "ctime": 200},
	})
	p := newTestProvider(t, m, nil)

	b, err := p.GetLatestBackup(context.Background(), "101")
	if err != nil {
		t.Fatalf("GetLatestBackup: %v", err)
	}
	if b.ID != "local:backup/new" {
		t.Errorf("GetLatestBackup returned %q, want the newest backup", b.ID)
	}
}
