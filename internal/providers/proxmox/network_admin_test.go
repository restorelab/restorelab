package proxmox

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureIsolatedBridgeCreatesAndApplies(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{})
	m.on("POST", "/api2/json/nodes/pve1/network", 200, nil)
	m.on("PUT", "/api2/json/nodes/pve1/network", 200, nil)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
		Node:   "pve1",
		Bridge: "vmbr99",
		Apply:  true,
	})
	if err != nil {
		t.Fatalf("EnsureIsolatedBridge: %v", err)
	}
	if !result.Applied {
		t.Error("expected Applied = true")
	}
	if result.PendingApply {
		t.Error("expected PendingApply = false once applied")
	}

	writes := writesOnly(m.recorded())
	if len(writes) != 2 {
		t.Fatalf("got %d writes, want 2 (create + apply):\n%+v", len(writes), writes)
	}

	create := writes[0]
	if create.Method != "POST" || create.Path != "/api2/json/nodes/pve1/network" {
		t.Errorf("create write = %s %s, want POST /api2/json/nodes/pve1/network", create.Method, create.Path)
	}
	wantForm := map[string]string{
		"iface":      "vmbr99",
		"type":       "bridge",
		"autostart":  "1",
		"bridge_stp": "off",
		"bridge_fd":  "0",
		"comments":   defaultBridgeComment,
	}
	for k, v := range wantForm {
		if got := create.Form.Get(k); got != v {
			t.Errorf("create form[%q] = %q, want %q", k, got, v)
		}
	}
	if _, present := create.Form["bridge_ports"]; !present {
		t.Error("create form must explicitly include bridge_ports")
	}
	if got := create.Form.Get("bridge_ports"); got != "" {
		t.Errorf("create form[bridge_ports] = %q, want empty", got)
	}

	apply := writes[1]
	if apply.Method != "PUT" || apply.Path != "/api2/json/nodes/pve1/network" {
		t.Errorf("apply write = %s %s, want PUT /api2/json/nodes/pve1/network", apply.Method, apply.Path)
	}
}

func TestEnsureIsolatedBridgeCustomComment(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1b", "csrf-1b")
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{})
	m.on("POST", "/api2/json/nodes/pve1/network", 200, nil)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if _, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
		Node:    "pve1",
		Bridge:  "vmbr99",
		Comment: "custom marker",
	}); err != nil {
		t.Fatalf("EnsureIsolatedBridge: %v", err)
	}

	writes := writesOnly(m.recorded())
	if len(writes) != 1 {
		t.Fatalf("got %d writes, want 1", len(writes))
	}
	if got := writes[0].Form.Get("comments"); got != "custom marker" {
		t.Errorf("comments = %q, want %q", got, "custom marker")
	}
}

func TestEnsureIsolatedBridgeApplyFalseIssuesNoPUT(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-2", "csrf-2")
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{})
	m.on("POST", "/api2/json/nodes/pve1/network", 200, nil)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
		Node:   "pve1",
		Bridge: "vmbr99",
		// Apply left false.
	})
	if err != nil {
		t.Fatalf("EnsureIsolatedBridge: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied = false")
	}
	if !result.PendingApply {
		t.Error("expected PendingApply = true")
	}
	for _, r := range m.recorded() {
		if r.Method == "PUT" {
			t.Errorf("unexpected PUT with Apply=false: %s %s", r.Method, r.Path)
		}
	}
}

func TestEnsureIsolatedBridgeAlreadyExistsPortlessAddressless(t *testing.T) {
	tests := []struct {
		name        string
		bridgePorts string
	}{
		{"empty bridge_ports", ""},
		{"literal none", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			mockTicket(m, "tkt-3", "csrf-3")
			m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{
				{"iface": "vmbr99", "type": "bridge", "bridge_ports": tt.bridgePorts},
			})

			c := newTestAdminClient(t, m, "admin-pw", nil)
			mustLogin(t, c)

			result, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
				Node:   "pve1",
				Bridge: "vmbr99",
				Apply:  true,
			})
			if err != nil {
				t.Fatalf("EnsureIsolatedBridge: %v", err)
			}
			if len(result.Steps) == 0 || result.Steps[0].Status != "already exists" {
				t.Errorf("steps = %+v, want first step status \"already exists\"", result.Steps)
			}
			if writes := writesOnly(m.recorded()); len(writes) != 0 {
				t.Errorf("expected zero writes, got %+v", writes)
			}
		})
	}
}

func TestEnsureIsolatedBridgeRefusesUnsafeExistingInterface(t *testing.T) {
	tests := []struct {
		name    string
		iface   map[string]any
		wantErr string
	}{
		{
			name:    "has bridge ports",
			iface:   map[string]any{"iface": "vmbr99", "type": "bridge", "bridge_ports": "eno1"},
			wantErr: "bridge_ports",
		},
		{
			name:    "has cidr",
			iface:   map[string]any{"iface": "vmbr99", "type": "bridge", "bridge_ports": "", "cidr": "10.0.0.1/24"},
			wantErr: "address",
		},
		{
			name:    "has address",
			iface:   map[string]any{"iface": "vmbr99", "type": "bridge", "bridge_ports": "", "address": "10.0.0.1"},
			wantErr: "address",
		},
		{
			name:    "has gateway",
			iface:   map[string]any{"iface": "vmbr99", "type": "bridge", "bridge_ports": "", "gateway": "10.0.0.1"},
			wantErr: "gateway",
		},
		{
			name:    "not a bridge",
			iface:   map[string]any{"iface": "vmbr99", "type": "eth"},
			wantErr: "not a bridge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			mockTicket(m, "tkt-4", "csrf-4")
			m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{tt.iface})

			c := newTestAdminClient(t, m, "admin-pw", nil)
			mustLogin(t, c)

			_, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
				Node:   "pve1",
				Bridge: "vmbr99",
				Apply:  true,
			})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantErr)
			}
			if writes := writesOnly(m.recorded()); len(writes) != 0 {
				t.Errorf("unexpected write against an unsafe existing interface: %+v", writes)
			}
		})
	}
}

func TestValidateBridgeNameRejectsInvalidNames(t *testing.T) {
	names := []string{"br0", "vmbr", "vmbr99999", "vmbr-1", ""}
	for _, name := range names {
		t.Run("name="+name, func(t *testing.T) {
			m := newMockServer(t)
			// Deliberately no ticket mock and no Login call: an invalid
			// bridge name must be rejected before any request, including
			// authentication, is attempted.
			c := newTestAdminClient(t, m, "admin-pw", nil)

			_, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
				Node:   "pve1",
				Bridge: name,
			})
			if err == nil {
				t.Fatalf("expected an error for bridge name %q, got nil", name)
			}
			if reqs := m.recorded(); len(reqs) != 0 {
				t.Errorf("bridge name %q: expected zero requests, got %+v", name, reqs)
			}
		})
	}
}

func TestEnsureIsolatedBridgeDryRunIssuesZeroWrites(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-5", "csrf-5")
	m.on("GET", "/api2/json/nodes/pve1/network", 200, []map[string]any{})
	// No POST/PUT handlers registered: any write attempt 501s.

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.EnsureIsolatedBridge(context.Background(), BridgeOptions{
		Node:   "pve1",
		Bridge: "vmbr99",
		Apply:  true,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("EnsureIsolatedBridge (dry run): %v", err)
	}
	// Every step must read as hypothetical: a status that looks like an
	// accomplished action is how a dry run lies to someone.
	for _, s := range result.Steps {
		if !strings.HasPrefix(s.Status, "would ") {
			t.Errorf("step %q status = %q, want a \"would ...\" status under DryRun", s.Description, s.Status)
		}
	}
	for _, r := range m.recorded() {
		if r.Method == "POST" || r.Method == "PUT" {
			if r.Path == "/api2/json/access/ticket" {
				continue // Login itself precedes DryRun and is unavoidable
			}
			t.Errorf("dry run issued a write: %s %s", r.Method, r.Path)
		}
	}
}

func TestRevertPendingNetworkIssuesDelete(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-6", "csrf-6")
	m.on("DELETE", "/api2/json/nodes/pve1/network", 200, nil)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if err := c.RevertPendingNetwork(context.Background(), "pve1"); err != nil {
		t.Fatalf("RevertPendingNetwork: %v", err)
	}

	var found bool
	for _, r := range m.recorded() {
		if r.Method == "DELETE" && r.Path == "/api2/json/nodes/pve1/network" {
			found = true
		}
	}
	if !found {
		t.Error("expected a DELETE /api2/json/nodes/pve1/network request")
	}
}
