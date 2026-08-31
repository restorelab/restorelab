package proxmox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// qemuResource registers the /cluster/resources response resolve() needs to
// find workload id "101" as a qemu VM on node "pve1".
func qemuResource(m *mockServer) {
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "qemu", "vmid": 101, "name": "vm101", "node": "pve1", "status": "running"},
	})
}

func TestExecInGuestCommandFieldRepeatedInOrder(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantInput bool
	}{
		{name: "no input", input: "", wantInput: false},
		{name: "with input", input: "hello stdin", wantInput: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			qemuResource(m)
			m.on("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 200, map[string]any{"pid": 555})
			m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/exec-status", 200, map[string]any{
				"exited": 1, "exitcode": 0,
			})
			p := newTestProvider(t, m, nil)
			p.pollInterval = time.Millisecond

			argv := []string{"systemctl", "is-active", "postgresql"}
			_, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: argv, Input: tt.input})
			if err != nil {
				t.Fatalf("ExecInGuest: %v", err)
			}

			var postReq *recordedRequest
			for _, r := range m.recorded() {
				if r.Method == "POST" && strings.HasSuffix(r.Path, "/agent/exec") {
					rc := r
					postReq = &rc
					break
				}
			}
			if postReq == nil {
				t.Fatal("no POST .../agent/exec request recorded")
			}

			gotCommand := postReq.Form["command"]
			if !reflect.DeepEqual(gotCommand, argv) {
				t.Errorf("command form field = %v, want %v (repeated, in order)", gotCommand, argv)
			}

			_, hasInput := postReq.Form["input-data"]
			if hasInput != tt.wantInput {
				t.Errorf("input-data present = %v, want %v (form: %v)", hasInput, tt.wantInput, postReq.Form)
			}
			if tt.wantInput && postReq.Form.Get("input-data") != tt.input {
				t.Errorf("input-data = %q, want %q", postReq.Form.Get("input-data"), tt.input)
			}
		})
	}
}

func TestExecInGuestPollsUntilExited(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 200, map[string]any{"pid": 42})
	m.onSequence("GET", "/api2/json/nodes/pve1/qemu/101/agent/exec-status",
		jsonRoute(200, map[string]any{"exited": 0}),
		jsonRoute(200, map[string]any{"exited": 0}),
		jsonRoute(200, map[string]any{"exited": 1, "exitcode": 3}),
	)
	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Millisecond

	res, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: []string{"false"}})
	// A non-zero exit code is a successful call, not an error: this is the
	// whole point of ExecResult.ExitCode existing.
	if err != nil {
		t.Fatalf("ExecInGuest: unexpected error for a non-zero exit code: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}

	polls := 0
	for _, r := range m.recorded() {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/agent/exec-status") {
			polls++
		}
	}
	if polls != 3 {
		t.Errorf("expected 3 exec-status polls, got %d", polls)
	}
}

func TestExecInGuestStdoutStderrAndTruncatedFlag(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 200, map[string]any{"pid": 7})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/exec-status", 200, map[string]any{
		"exited": 1, "exitcode": 0,
		"out-data": "hello out", "err-data": "hello err",
		"out-truncated": 1, "err-truncated": 0,
		"signal": "",
	})
	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Millisecond

	res, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: []string{"echo"}})
	if err != nil {
		t.Fatalf("ExecInGuest: %v", err)
	}
	if res.Stdout != "hello out" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello out")
	}
	if res.Stderr != "hello err" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "hello err")
	}
	if !res.Truncated {
		t.Error("expected Truncated=true from out-truncated=1")
	}
	if res.Signal != "" {
		t.Errorf("Signal = %q, want empty", res.Signal)
	}
}

func TestExecInGuestMaxOutputBytesTruncates(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 200, map[string]any{"pid": 7})
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/exec-status", 200, map[string]any{
		"exited": 1, "exitcode": 0,
		"out-data": "0123456789", "err-data": "",
	})
	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Millisecond

	res, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{
		Argv: []string{"echo"}, MaxOutputBytes: 5,
	})
	if err != nil {
		t.Fatalf("ExecInGuest: %v", err)
	}
	if res.Stdout != "01234" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "01234")
	}
	if !res.Truncated {
		t.Error("expected Truncated=true from MaxOutputBytes cap")
	}
}

func TestExecInGuestAgentUnavailable(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.onError("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 500, "VM 101 qmp command 'guest-exec' failed - QEMU guest agent is not running")
	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Millisecond

	_, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrGuestAgentUnavailable) {
		t.Errorf("expected core.ErrGuestAgentUnavailable, got %v", err)
	}
	if core.IsRetryable(err) {
		t.Errorf("agent-unavailable must not be retryable (retrying wastes time against a guest with no agent), got %v", err)
	}
}

func TestExecInGuestUnauthorized(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.onError("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 403, "Permission check failed")
	p := newTestProvider(t, m, nil)

	_, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("expected core.ErrUnauthorized, got %v", err)
	}
}

func TestExecInGuestContextCancellationDuringPoll(t *testing.T) {
	m := newMockServer(t)
	qemuResource(m)
	m.on("POST", "/api2/json/nodes/pve1/qemu/101/agent/exec", 200, map[string]any{"pid": 9})
	// Always "still running": ExecInGuest must never see exited=1 and must
	// come back only because the context expired.
	m.on("GET", "/api2/json/nodes/pve1/qemu/101/agent/exec-status", 200, map[string]any{"exited": 0})

	p := newTestProvider(t, m, nil)
	p.pollInterval = time.Hour // must never actually be waited out

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.ExecInGuest(ctx, "101", core.ExecRequest{Argv: []string{"sleep", "999"}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from context cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ExecInGuest did not return promptly on ctx cancellation: took %v", elapsed)
	}
}

func TestExecInGuestLXCUnsupported(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/cluster/resources", 200, []map[string]any{
		{"type": "lxc", "vmid": 201, "name": "ct201", "node": "pve1", "status": "running"},
	})
	p := newTestProvider(t, m, nil)

	_, err := p.ExecInGuest(context.Background(), "201", core.ExecRequest{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestExecInGuestEmptyArgvRejected(t *testing.T) {
	p := newTestProvider(t, newMockServer(t), nil)

	_, err := p.ExecInGuest(context.Background(), "101", core.ExecRequest{Argv: nil})
	if err == nil {
		t.Fatal("expected an error for empty Argv")
	}
	// This is a programming error, not an agent/auth condition: it must not
	// be mistaken for either sentinel.
	if errors.Is(err, core.ErrGuestAgentUnavailable) || errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrUnsupported) {
		t.Errorf("empty Argv error should be a plain error, got %v", err)
	}
}
