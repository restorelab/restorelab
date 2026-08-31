package proxmox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

// The payloads below are verbatim shapes of what qemu-guest-agent answers on
// each platform, minus the fields RestoreLab does not read.
func TestGuestOSFamilies(t *testing.T) {
	tests := []struct {
		name           string
		result         map[string]any
		wantFamily     string
		wantID         string
		wantVersion    string
		wantDescribing string
	}{
		{
			name: "debian",
			result: map[string]any{
				"id":          "debian",
				"kernel-name": "Linux",
				"name":        "Debian GNU/Linux",
				"pretty-name": "Debian GNU/Linux 12 (bookworm)",
				"version":     "12 (bookworm)",
				"version-id":  "12",
			},
			wantFamily:     core.GuestOSLinux,
			wantID:         "debian",
			wantVersion:    "12 (bookworm)",
			wantDescribing: "Debian GNU/Linux 12 (bookworm)",
		},
		{
			name: "windows",
			result: map[string]any{
				"id":          "mswindows",
				"kernel-name": "Windows",
				"name":        "Microsoft Windows",
				"pretty-name": "Windows Server 2022 Standard",
				"version":     "Microsoft Windows Server 2022",
				"version-id":  "2022",
			},
			wantFamily:     core.GuestOSWindows,
			wantID:         "mswindows",
			wantVersion:    "Microsoft Windows Server 2022",
			wantDescribing: "Windows Server 2022 Standard",
		},
		{
			// A guest agent that reports neither Windows nor a Linux kernel
			// must leave the family unknown rather than be assumed Unix: the
			// whole point of the field is to let the caller choose its own
			// fallback knowingly.
			name: "unknown family stays empty",
			result: map[string]any{
				"id":          "freebsd",
				"kernel-name": "FreeBSD",
				"name":        "FreeBSD",
				"version-id":  "14.0",
			},
			wantFamily:     "",
			wantID:         "freebsd",
			wantVersion:    "14.0",
			wantDescribing: "FreeBSD 14.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			qemuResource(m)
			m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/get-osinfo", 200, map[string]any{"result": tt.result})
			p := newTestProvider(t, m, nil)

			got, err := p.GuestOS(context.Background(), "101")
			if err != nil {
				t.Fatalf("GuestOS: %v", err)
			}
			if got.Family != tt.wantFamily {
				t.Errorf("Family = %q, want %q", got.Family, tt.wantFamily)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.String() != tt.wantDescribing {
				t.Errorf("String() = %q, want %q", got.String(), tt.wantDescribing)
			}
		})
	}
}

// A guest that is still booting answers get-osinfo with the same untyped 500
// PVE uses for a missing agent. Callers distinguish "I could not ask" from
// "the answer is bad" through ErrGuestAgentUnavailable, so this endpoint must
// map it exactly like ExecInGuest does.
func TestGuestOSAgentDownIsGuestAgentUnavailable(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/get-osinfo", 500, "QEMU guest agent is not running")
	p := newTestProvider(t, m, nil)

	_, err := p.GuestOS(context.Background(), "101")
	if !errors.Is(err, core.ErrGuestAgentUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrGuestAgentUnavailable", err)
	}
	if core.IsRetryable(err) {
		t.Fatalf("error = %v, want it NOT retryable: a guest with no agent will not grow one", err)
	}
}

func TestGuestOSRejectsNonQemuWorkload(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "lxc", "vmid": 201, "name": "ct201", "node": "pve1", "status": "running"},
	})
	p := newTestProvider(t, m, nil)

	_, err := p.GuestOS(context.Background(), "201")
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "lxc") {
		t.Errorf("error should name the workload kind, got: %v", err)
	}
}

// An agent that answers with an empty result is not an error - it is a guest
// that knows nothing about itself. Family stays empty so the caller falls
// back deliberately.
func TestGuestOSEmptyResult(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/get-osinfo", 200, map[string]any{"result": map[string]any{}})
	p := newTestProvider(t, m, nil)

	got, err := p.GuestOS(context.Background(), "101")
	if err != nil {
		t.Fatalf("GuestOS: %v", err)
	}
	if got.Family != "" {
		t.Errorf("Family = %q, want empty", got.Family)
	}
	if got.String() != "unknown" {
		t.Errorf("String() = %q, want %q", got.String(), "unknown")
	}
}
