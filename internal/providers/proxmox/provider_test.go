package proxmox

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func TestListNodes(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes", 200, []map[string]any{
		{"node": "pve1", "status": "online", "cpu": 0.05, "maxcpu": 8, "mem": 1000, "maxmem": 8000, "disk": 100, "maxdisk": 1000},
		{"node": "pve2", "status": "offline", "cpu": 0, "maxcpu": 4, "mem": 0, "maxmem": 4000, "disk": 0, "maxdisk": 500},
	})
	p := newTestProvider(t, m, nil)

	nodes, err := p.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if !nodes[0].Online || nodes[0].CPUCores != 8 || nodes[0].MemoryTotalBytes != 8000 || nodes[0].DiskUsedBytes != 100 {
		t.Errorf("node0 mismapped: %+v", nodes[0])
	}
	if nodes[1].Online {
		t.Errorf("node1 should be offline: %+v", nodes[1])
	}
}

func TestListWorkloads(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "name": "vm101", "node": "pve1", "status": "running", "maxcpu": 2, "maxmem": 2147483648, "maxdisk": 34359738368, "template": 0, "tags": "restorelab;prod"},
		{"type": "lxc", "vmid": 201, "name": "ct201", "node": "pve1", "status": "stopped", "maxcpu": 1, "maxmem": 536870912, "maxdisk": 8589934592, "template": 0, "tags": ""},
		{"type": "qemu", "vmid": 102, "name": "tmpl", "node": "pve1", "status": "stopped", "maxcpu": 1, "maxmem": 1073741824, "maxdisk": 10737418240, "template": 1, "tags": ""},
	})
	p := newTestProvider(t, m, nil)

	workloads, err := p.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(workloads) != 3 {
		t.Fatalf("expected 3 workloads, got %d", len(workloads))
	}

	vm := workloads[0]
	if vm.Kind != core.WorkloadKindVM || vm.ID != "101" || !vm.Managed || vm.PowerState != core.PowerStateRunning {
		t.Errorf("vm mismapped: %+v", vm)
	}
	if len(vm.Tags) != 2 || vm.Tags[0] != "restorelab" || vm.Tags[1] != "prod" {
		t.Errorf("vm tags mismapped: %+v", vm.Tags)
	}

	ct := workloads[1]
	if ct.Kind != core.WorkloadKindContainer || ct.Managed {
		t.Errorf("ct mismapped: %+v", ct)
	}

	tmpl := workloads[2]
	if !tmpl.Template || tmpl.Managed {
		t.Errorf("template mismapped: %+v", tmpl)
	}
}

func TestGetStatusWithWorkingAgent(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "name": "vm101", "node": "pve1", "status": "running"},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/status/current", 200, map[string]any{
		"status": "running", "uptime": 3600, "cpu": 0.1, "mem": 123456,
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces", 200, map[string]any{
		"result": []map[string]any{
			{"name": "eth0", "ip-addresses": []map[string]any{
				{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"},
				{"ip-address": "169.254.1.1", "ip-address-type": "ipv4"},
				{"ip-address": "fe80::1", "ip-address-type": "ipv6"},
				{"ip-address": "::1", "ip-address-type": "ipv6"},
				{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
			}},
		},
	})
	p := newTestProvider(t, m, nil)

	status, err := p.GetStatus(context.Background(), "101")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.AgentReady {
		t.Fatal("expected AgentReady=true")
	}
	if status.Uptime != 3600*time.Second {
		t.Errorf("uptime = %v", status.Uptime)
	}
	if len(status.IPs) != 1 || status.IPs[0] != "10.0.0.5" {
		t.Errorf("expected only 10.0.0.5 to survive filtering, got %v", status.IPs)
	}
}

func TestGetStatusAgentFailureIsNotFatal(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "name": "vm101", "node": "pve1", "status": "running"},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/status/current", 200, map[string]any{
		"status": "running", "uptime": 10, "cpu": 0, "mem": 0,
	})
	m.onError("GET", "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces", 500, "guest agent is not running")
	p := newTestProvider(t, m, nil)

	status, err := p.GetStatus(context.Background(), "101")
	if err != nil {
		t.Fatalf("GetStatus must not fail when the agent is unreachable: %v", err)
	}
	if status.AgentReady {
		t.Error("expected AgentReady=false")
	}
	if len(status.IPs) != 0 {
		t.Errorf("expected no IPs, got %v", status.IPs)
	}
}

func TestAllocateWorkloadIDWithinRange(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/nextid", 200, "9050")
	p := newTestProvider(t, m, nil)

	id, err := p.AllocateWorkloadID(context.Background())
	if err != nil {
		t.Fatalf("AllocateWorkloadID: %v", err)
	}
	if id != "9050" {
		t.Errorf("id = %q, want 9050", id)
	}
	if len(m.recorded()) != 1 {
		t.Errorf("expected a single request (no probing needed), got %d", len(m.recorded()))
	}
}

func TestAllocateWorkloadIDProbesWhenOutOfRange(t *testing.T) {
	m := newMockServer(t)
	m.onFunc("GET", "/api2/json/cluster/nextid", func(r *http.Request) mockRoute {
		vmid := r.URL.Query().Get("vmid")
		if vmid == "" {
			return jsonRoute(200, "105") // cluster-wide next id, outside our temp range
		}
		n, _ := strconv.Atoi(vmid)
		if n == 9002 {
			return jsonRoute(200, strconv.Itoa(n))
		}
		return mockRoute{status: 400, body: []byte(`{"data":null,"errors":{"vmid":"already exists"}}`)}
	})
	p := newTestProvider(t, m, func(c *Config) { c.TempIDMin = 9000; c.TempIDMax = 9005 })

	id, err := p.AllocateWorkloadID(context.Background())
	if err != nil {
		t.Fatalf("AllocateWorkloadID: %v", err)
	}
	if id != "9002" {
		t.Errorf("id = %q, want 9002", id)
	}
}

func TestAllocateWorkloadIDRangeExhausted(t *testing.T) {
	m := newMockServer(t)
	m.onFunc("GET", "/api2/json/cluster/nextid", func(r *http.Request) mockRoute {
		vmid := r.URL.Query().Get("vmid")
		if vmid == "" {
			return jsonRoute(200, "105")
		}
		return mockRoute{status: 400, body: []byte(`{"data":null,"errors":{"vmid":"already exists"}}`)}
	})
	p := newTestProvider(t, m, func(c *Config) { c.TempIDMin = 9000; c.TempIDMax = 9002 })

	_, err := p.AllocateWorkloadID(context.Background())
	if err == nil {
		t.Fatal("expected an error when the whole range is taken")
	}
}

func TestRestoreFormParameters(t *testing.T) {
	m := newMockServer(t)
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:00001234:0000ABCD:0000000:qmrestore:9000:root@pam:")
	p := newTestProvider(t, m, nil)

	backup := core.Backup{
		ID:   "local:backup/vzdump-qemu-101-2026_08_31-03_00_00.vma.zst",
		Node: "pve1",
	}
	opts := core.RestoreOptions{
		TargetWorkloadID: "9000",
		Node:             "pve1",
		Storage:          "local-lvm",
		BandwidthKiBps:   1024,
	}

	job, err := p.Restore(context.Background(), backup, opts)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if job.ID != "UPID:pve1:00001234:0000ABCD:0000000:qmrestore:9000:root@pam:" {
		t.Errorf("job.ID = %q", job.ID)
	}
	if job.Node != "pve1" || job.WorkloadID != "9000" {
		t.Errorf("job mismapped: %+v", job)
	}

	reqs := m.recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	form := reqs[0].Form
	assertForm(t, form, "vmid", "9000")
	assertForm(t, form, "archive", backup.ID)
	assertForm(t, form, "storage", "local-lvm")
	assertForm(t, form, "unique", "1")
	assertForm(t, form, "start", "0")
	assertForm(t, form, "bwlimit", "1024")
	if form.Has("force") {
		t.Errorf("Restore must never send force=1, got form=%v", form)
	}
}

func TestRestoreOmitsStorageAndBandwidthWhenUnset(t *testing.T) {
	m := newMockServer(t)
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:xxx")
	p := newTestProvider(t, m, nil)

	_, err := p.Restore(context.Background(), core.Backup{ID: "local:backup/x", Node: "pve1"}, core.RestoreOptions{TargetWorkloadID: "9000"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	form := m.recorded()[0].Form
	if form.Has("storage") {
		t.Errorf("storage should be omitted when empty, got %v", form)
	}
	if form.Has("bwlimit") {
		t.Errorf("bwlimit should be omitted when zero, got %v", form)
	}
	if form.Has("force") {
		t.Errorf("force must never be sent")
	}
}

func assertForm(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Errorf("form[%q] = %q, want %q", key, got, want)
	}
}

func TestFinalizeRestoreHardensNetworkAndMetadata(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/qemu/9000/config", 200, map[string]any{
		"net0":   "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
		"net1":   "virtio=11:22:33:44:55:66,bridge=vmbr1",
		"net2":   "e1000=AA:AA:AA:AA:AA:AA,bridge=vmbr2",
		"cores":  2,
		"memory": 2048,
		"name":   "prod-vm",
	})
	m.on("POST", "/api2/json/nodes/pve1/qemu/9000/config", 200, nil)
	p := newTestProvider(t, m, nil)

	opts := core.RestoreOptions{
		TargetWorkloadID: "9000",
		Node:             "pve1",
		Name:             "restored-vm",
		CPULimit:         4,
		MemoryLimitMB:    4096,
		Network: core.NetworkConfig{
			Bridge:   "vmbr99",
			VLANTag:  50,
			Firewall: true,
			Isolated: true,
		},
		Metadata: map[string]string{
			core.MetadataManaged:       "true",
			core.MetadataRecoveryRunID: "run-1",
		},
	}

	if err := p.FinalizeRestore(context.Background(), opts); err != nil {
		t.Fatalf("FinalizeRestore: %v", err)
	}

	var form url.Values
	for _, r := range m.recorded() {
		if r.Method == "POST" {
			form = r.Form
		}
	}
	if form == nil {
		t.Fatal("no POST request recorded")
	}

	assertForm(t, form, "net0", "virtio,bridge=vmbr99,tag=50,firewall=1")
	assertForm(t, form, "delete", "net1,net2")
	assertForm(t, form, "onboot", "0")
	assertForm(t, form, "protection", "0")
	assertForm(t, form, "name", "restored-vm")
	assertForm(t, form, "cores", "4")
	assertForm(t, form, "memory", "4096")
	assertForm(t, form, "tags", "restorelab")

	desc := form.Get("description")
	if !containsSubstr(desc, "restorelab_managed=true") || !containsSubstr(desc, "restorelab_run_id=run-1") {
		t.Errorf("description missing metadata lines: %q", desc)
	}
}

func TestFinalizeRestoreWithoutVLANOrFirewall(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/qemu/9000/config", 200, map[string]any{"net0": "virtio=..,bridge=vmbr0"})
	m.on("POST", "/api2/json/nodes/pve1/qemu/9000/config", 200, nil)
	p := newTestProvider(t, m, nil)

	opts := core.RestoreOptions{
		TargetWorkloadID: "9000",
		Node:             "pve1",
		Network:          core.NetworkConfig{Bridge: "vmbr99"},
	}
	if err := p.FinalizeRestore(context.Background(), opts); err != nil {
		t.Fatalf("FinalizeRestore: %v", err)
	}
	form := m.recorded()[1].Form
	assertForm(t, form, "net0", "virtio,bridge=vmbr99")
	if form.Has("delete") {
		t.Errorf("no interface should be deleted when only net0 exists, got %v", form)
	}
}

func TestWaitForTaskSuccess(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/tasks/UPID:xyz/status", 200, map[string]any{
		"status": "stopped", "exitstatus": "OK",
	})
	p := newTestProvider(t, m, nil)

	ts, err := p.WaitForTask(context.Background(), "pve1", "UPID:xyz")
	if err != nil {
		t.Fatalf("WaitForTask: %v", err)
	}
	if !ts.Success || ts.Running {
		t.Errorf("task state mismapped: %+v", ts)
	}
}

func TestWaitForTaskFailure(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/tasks/UPID:xyz/status", 200, map[string]any{
		"status": "stopped", "exitstatus": "backup archive not found",
	})
	p := newTestProvider(t, m, nil)

	ts, err := p.WaitForTask(context.Background(), "pve1", "UPID:xyz")
	if err != nil {
		t.Fatalf("WaitForTask should report failure via TaskState, not an error: %v", err)
	}
	if ts.Success {
		t.Error("expected Success=false")
	}
	if ts.ExitCode != "backup archive not found" {
		t.Errorf("ExitCode = %q", ts.ExitCode)
	}
}

func TestWaitForTaskPollsUntilStopped(t *testing.T) {
	m := newMockServer(t)
	m.onSequence("GET", "/api2/json/nodes/pve1/tasks/UPID:xyz/status",
		jsonRoute(200, map[string]any{"status": "running"}),
		jsonRoute(200, map[string]any{"status": "running"}),
		jsonRoute(200, map[string]any{"status": "stopped", "exitstatus": "OK"}),
	)
	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Millisecond

	ts, err := p.WaitForTask(context.Background(), "pve1", "UPID:xyz")
	if err != nil {
		t.Fatalf("WaitForTask: %v", err)
	}
	if !ts.Success {
		t.Errorf("expected success, got %+v", ts)
	}
	if got := len(m.recorded()); got != 3 {
		t.Errorf("expected 3 polls, got %d", got)
	}
}

func TestDeleteRefusesUnmanagedWorkload(t *testing.T) {
	m := newMockServer(t)
	// In the reserved range, so the ID gate passes and the metadata gate is
	// the one under test.
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 9500, "name": "someone-elses-vm", "node": "pve1", "status": "running", "tags": ""},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/9500/config", 200, map[string]any{
		"tags": "", "description": "not managed by restorelab",
	})
	p := newTestProvider(t, m, nil)

	err := p.Delete(context.Background(), "9500")
	if !errors.Is(err, core.ErrNotManaged) {
		t.Fatalf("expected core.ErrNotManaged, got %v", err)
	}

	for _, r := range m.recorded() {
		if r.Method == "DELETE" {
			t.Fatalf("Delete must not issue a destructive request for an unmanaged workload, got %+v", r)
		}
		if r.Method == "POST" {
			t.Fatalf("Delete must not stop/start an unmanaged workload, got %+v", r)
		}
	}
	// Only the safety-check reads should have happened.
	if got := len(m.recorded()); got != 2 {
		t.Errorf("expected exactly 2 read-only requests, got %d: %+v", got, m.recorded())
	}
}

// A tag is not proof of ownership: a human can put "restorelab" on any VM,
// and a clone carries it along. Only the metadata FinalizeRestore stamps
// authorises a destroy.
func TestDeleteRefusesTagOnlyWorkload(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 9000, "node": "pve1", "tags": "restorelab"},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/9000/config", 200, map[string]any{
		"tags": "restorelab", "description": "",
	})
	p := newTestProvider(t, m, nil)

	err := p.Delete(context.Background(), "9000")
	if !errors.Is(err, core.ErrNotManaged) {
		t.Fatalf("Delete() error = %v, want core.ErrNotManaged", err)
	}
	for _, r := range m.recorded() {
		if r.Method == "DELETE" || r.Method == "POST" {
			t.Fatalf("no destructive request may be issued, got %+v", r)
		}
	}
}

// The reserved ID range is an independent second gate: metadata alone must
// not be able to authorise destroying a workload outside it.
func TestDeleteRefusesWorkloadOutsideTempRange(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "node": "pve1", "tags": "restorelab"},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/config", 200, map[string]any{
		"tags": "restorelab", "description": "restorelab_managed=true",
	})
	p := newTestProvider(t, m, nil)

	err := p.Delete(context.Background(), "101")
	if !errors.Is(err, core.ErrNotManaged) {
		t.Fatalf("Delete() error = %v, want core.ErrNotManaged", err)
	}
	if !strings.Contains(err.Error(), "reserved temporary id range") {
		t.Errorf("error should explain the range gate, got %v", err)
	}
	if len(m.recorded()) != 0 {
		t.Fatalf("the range gate must refuse before any API call, got %+v", m.recorded())
	}
}

func TestDeleteManagedWorkloadByDescription(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 9001, "node": "pve1", "tags": ""},
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/9001/config", 200, map[string]any{
		"tags": "", "description": "restorelab_managed=true\nrestorelab_run_id=run-9",
	})
	m.on("GET", "/api2/json/nodes/pve1/qemu/9001/status/current", 200, map[string]any{"status": "stopped"})
	m.on("DELETE", "/api2/json/nodes/pve1/qemu/9001", 200, "UPID:pve1:del:9001:root@pam:")
	m.on("GET", "/api2/json/nodes/pve1/tasks/UPID:pve1:del:9001:root@pam:/status", 200, map[string]any{
		"status": "stopped", "exitstatus": "OK",
	})
	p := newTestProvider(t, m, nil)

	if err := p.Delete(context.Background(), "9001"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestNodeCapacity(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/status", 200, map[string]any{
		"cpu":     0.2,
		"cpuinfo": map[string]any{"cpus": 16},
		"memory":  map[string]any{"total": 1000, "used": 400, "free": 600},
		"rootfs":  map[string]any{"total": 5000, "used": 1000, "free": 4000},
	})
	p := newTestProvider(t, m, nil)

	node, err := p.NodeCapacity(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeCapacity: %v", err)
	}
	if node.CPUCores != 16 || node.MemoryTotalBytes != 1000 || node.MemoryUsedBytes != 400 || node.DiskTotalBytes != 5000 {
		t.Errorf("node mismapped: %+v", node)
	}
}

func TestValidateIsolationPassesForBridgeWithNoUplink(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{
		{"iface": "vmbr0", "bridge_ports": "eno1", "gateway": "10.0.0.1"},
		{"iface": "vmbr99", "bridge_ports": "", "type": "bridge"},
	})
	p := newTestProvider(t, m, nil)

	err := p.ValidateIsolation(context.Background(), "pve1", core.NetworkConfig{Bridge: "vmbr99"})
	if err != nil {
		t.Errorf("expected isolated bridge to pass, got %v", err)
	}
}

func TestValidateIsolationFailsForBridgeWithUplink(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{
		{"iface": "vmbr0", "bridge_ports": "eno1", "gateway": "10.0.0.1"},
	})
	p := newTestProvider(t, m, nil)

	err := p.ValidateIsolation(context.Background(), "pve1", core.NetworkConfig{Bridge: "vmbr0"})
	if !errors.Is(err, core.ErrNetworkNotIsolated) {
		t.Errorf("expected core.ErrNetworkNotIsolated, got %v", err)
	}
}

// The node's bridges are visible and the requested one is genuinely absent:
// that is a real misconfiguration, and a hard stop.
func TestValidateIsolationFailsWhenBridgeMissing(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{
		{"iface": "vmbr0", "type": "bridge", "bridge_ports": "eno1", "gateway": "10.0.0.1"},
		{"iface": "vmbr1", "type": "bridge", "bridge_ports": ""},
	})
	p := newTestProvider(t, m, nil)

	err := p.ValidateIsolation(context.Background(), "pve1", core.NetworkConfig{Bridge: "vmbr99"})
	if !errors.Is(err, core.ErrNetworkNotIsolated) {
		t.Errorf("expected core.ErrNetworkNotIsolated, got %v", err)
	}
}

// No bridge at all in the listing means the credentials cannot see them, not
// that the node has none. Proxmox does exactly this: on PVE 9.2.3 a token
// holding Sys.Audit on the node received only the physical NIC. Reporting it
// as "not isolated" would block a drill whose bridge is in fact correct, so
// the two cases must stay distinguishable to the caller.
func TestValidateIsolationReportsUnverifiableWhenNoBridgeIsVisible(t *testing.T) {
	tests := []struct {
		name    string
		entries []map[string]any
	}{
		{name: "empty listing", entries: []map[string]any{}},
		{
			name: "only physical interfaces",
			entries: []map[string]any{
				{"iface": "enp5s0", "type": "eth", "exists": 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			m.on("GET", "/api2/json/nodes/pve1/network", 200, tt.entries)
			p := newTestProvider(t, m, nil)

			err := p.ValidateIsolation(context.Background(), "pve1", core.NetworkConfig{Bridge: "vmbr99"})
			if !errors.Is(err, core.ErrIsolationUnverified) {
				t.Errorf("expected core.ErrIsolationUnverified, got %v", err)
			}
			if errors.Is(err, core.ErrNetworkNotIsolated) {
				t.Error("an unverifiable check must not masquerade as proven danger")
			}
		})
	}
}

func TestRestoreSendsThePoolOnlyWhenSet(t *testing.T) {
	newRestoreServer := func(t *testing.T) *mockServer {
		t.Helper()
		m := newMockServer(t)
		m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:qmrestore:9000:root@pam:")
		return m
	}

	t.Run("scoped to a pool", func(t *testing.T) {
		m := newRestoreServer(t)
		p := newTestProvider(t, m, nil)

		if _, err := p.Restore(context.Background(),
			core.Backup{ID: "pbs:backup/vm/101/2026-08-31T03:00:00Z", Node: "pve1"},
			core.RestoreOptions{TargetWorkloadID: "9000", Node: "pve1", Pool: "restorelab"},
		); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		form := restoreForm(t, m, "pve1")
		assertForm(t, form, "pool", "restorelab")
	})

	t.Run("no pool configured", func(t *testing.T) {
		m := newRestoreServer(t)
		p := newTestProvider(t, m, nil)

		if _, err := p.Restore(context.Background(),
			core.Backup{ID: "pbs:backup/vm/101/2026-08-31T03:00:00Z", Node: "pve1"},
			core.RestoreOptions{TargetWorkloadID: "9000", Node: "pve1"},
		); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		form := restoreForm(t, m, "pve1")
		if form.Has("pool") {
			t.Errorf("pool must not be sent when none is configured, got %q", form.Get("pool"))
		}
	})
}

// A restore must be isolated and owned from the instant the workload exists,
// not from the moment hardening happens to run afterwards.
//
// Both properties were learned from a real cluster: Proxmox validates the
// restored configuration as it creates the workload, so a backup referencing
// an SDN-managed bridge is refused outright (403 SDN.Use) unless the network
// is overridden in the create call; and when that create failed, the workload
// Proxmox had already made carried no ownership metadata, so cleanup refused
// to remove it and only a human could.
func TestRestoreIsolatesAndStampsAtCreation(t *testing.T) {
	m := newMockServer(t)
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:qmrestore:9000:root@pam:")
	p := newTestProvider(t, m, nil)

	_, err := p.Restore(context.Background(),
		core.Backup{ID: "local:backup/vzdump-qemu-104.vma.zst", Node: "pve1"},
		core.RestoreOptions{
			TargetWorkloadID: "9000",
			Node:             "pve1",
			Network:          core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
			Metadata: map[string]string{
				core.MetadataManaged:       "true",
				core.MetadataRecoveryRunID: "run-7",
				core.MetadataSourceID:      "104",
			},
		})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	form := restoreForm(t, m, "pve1")

	net0 := form.Get("net0")
	if !strings.Contains(net0, "bridge=vmbr99") {
		t.Errorf("net0 = %q, want the isolated bridge in the create call itself", net0)
	}
	if strings.Contains(net0, ":") {
		t.Errorf("net0 = %q, no MAC may be pinned: Proxmox must generate a fresh one", net0)
	}

	desc := form.Get("description")
	for _, want := range []string{core.MetadataManaged + "=true", core.MetadataRecoveryRunID + "=run-7"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description = %q, want it to carry %q from the start", desc, want)
		}
	}
	if got := form.Get("tags"); got != "restorelab" {
		t.Errorf("tags = %q, want restorelab", got)
	}
}

// A restore with no network configured must not invent one: that is the
// restore-onto-whatever-the-backup-had path, and it has to stay explicit.
func TestRestoreOmitsNetworkWhenNoneIsConfigured(t *testing.T) {
	m := newMockServer(t)
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:qmrestore:9000:root@pam:")
	p := newTestProvider(t, m, nil)

	if _, err := p.Restore(context.Background(),
		core.Backup{ID: "local:backup/x.vma.zst", Node: "pve1"},
		core.RestoreOptions{TargetWorkloadID: "9000", Node: "pve1"},
	); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if m.recorded()[0].Form.Has("net0") {
		t.Error("net0 must not be sent when no network was configured")
	}
}

// restoreForm returns the form of the restore POST, which is no longer the
// first request the provider makes: it reads the backup's configuration first.
func restoreForm(t *testing.T, m *mockServer, node string) url.Values {
	t.Helper()
	for _, r := range m.recorded() {
		if r.Method == http.MethodPost && r.Path == "/api2/json/nodes/"+node+"/qemu" {
			return r.Form
		}
	}
	t.Fatal("no restore request was made")
	return nil
}

// mockBackupConfig makes the extractconfig endpoint answer with a workload
// configuration carrying the given interfaces.
func mockBackupConfig(m *mockServer, node string, nets map[string]string) {
	lines := []string{"#qmdump#map:scsi0:drive-scsi0:local-zfs:", "boot: order=scsi0", "cores: 2"}
	for k, v := range nets {
		lines = append(lines, k+": "+v)
	}
	m.on("GET", "/api2/json/nodes/"+node+"/vzdump/extractconfig", 200, strings.Join(lines, "\n"))
}

// A workload with several interfaces must have every one of them neutralised
// in the create call. Overriding only net0 leaves the second NIC pointing at a
// production bridge, which Proxmox refuses when SDN permissions apply — and
// which would be a live production bridge where they do not.
func TestRestoreNeutralisesEveryInterfaceTheBackupCarries(t *testing.T) {
	m := newMockServer(t)
	mockBackupConfig(m, "pve1", map[string]string{
		"net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
		"net1": "virtio=11:22:33:44:55:66,bridge=vmbr1,tag=42",
		"net2": "e1000=AA:AA:AA:AA:AA:AA,bridge=vmbr2",
	})
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:qmrestore:9000:root@pam:")
	p := newTestProvider(t, m, nil)

	if _, err := p.Restore(context.Background(),
		core.Backup{ID: "local:backup/vzdump-qemu-104.vma.zst", Node: "pve1"},
		core.RestoreOptions{
			TargetWorkloadID: "9000",
			Node:             "pve1",
			Network:          core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
		},
	); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	form := restoreForm(t, m, "pve1")
	for _, iface := range []string{"net0", "net1", "net2"} {
		got := form.Get(iface)
		if !strings.Contains(got, "bridge=vmbr99") {
			t.Errorf("%s = %q, want it pointed at the isolated bridge", iface, got)
		}
	}
	for _, production := range []string{"vmbr0", "vmbr1", "vmbr2", "AA:BB:CC", "11:22:33"} {
		if strings.Contains(form.Encode(), production) {
			t.Errorf("the create call still carries %q from production", production)
		}
	}
}

// When the backup's configuration cannot be read, overriding net0 is what
// RestoreLab did before this existed: no worse than it was, and a workload
// with more interfaces fails loudly at restore rather than coming up attached
// to production.
func TestRestoreFallsBackToNet0WhenTheBackupConfigIsUnreadable(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/nodes/pve1/vzdump/extractconfig", 500, nil)
	m.on("POST", "/api2/json/nodes/pve1/qemu", 200, "UPID:pve1:qmrestore:9000:root@pam:")
	p := newTestProvider(t, m, nil)

	if _, err := p.Restore(context.Background(),
		core.Backup{ID: "local:backup/x.vma.zst", Node: "pve1"},
		core.RestoreOptions{
			TargetWorkloadID: "9000",
			Node:             "pve1",
			Network:          core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
		},
	); err != nil {
		t.Fatalf("Restore must still proceed: %v", err)
	}
	if got := restoreForm(t, m, "pve1").Get("net0"); !strings.Contains(got, "bridge=vmbr99") {
		t.Errorf("net0 = %q, want the isolated bridge", got)
	}
}

func TestParseConfigBlobIgnoresCommentsAndMapLines(t *testing.T) {
	config := parseConfigBlob("#qmdump#map:scsi0:drive-scsi0:local-zfs:\n# a comment\nname: tooling\nnet0: virtio,bridge=vmbr1\n\nagent: 1\n")
	if config["name"] != "tooling" || config["agent"] != "1" {
		t.Errorf("config = %v", config)
	}
	if _, ok := config["#qmdump#map"]; ok {
		t.Error("the qmdump map line must not become a config key")
	}
	if got := BackupNetworkDevices(config); len(got) != 1 || got[0] != "net0" {
		t.Errorf("BackupNetworkDevices() = %v", got)
	}
}
