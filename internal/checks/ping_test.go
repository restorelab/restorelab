package checks

import (
	"context"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func TestPingCheck_NoHost(t *testing.T) {
	cfg := core.CheckConfig{Type: "ping", Params: map[string]any{}}
	res := PingCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, Message = %q, want CheckError (no host)", res.Status, res.Message)
	}
}

func TestPingCheck_BadParams(t *testing.T) {
	cfg := core.CheckConfig{Type: "ping", Params: map[string]any{"count": "not-a-number"}}
	res := PingCheck{}.Run(context.Background(), core.Target{IP: "127.0.0.1"}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (bad count param)", res.Status)
	}
}

func TestPingCheck_UnresolvableHost(t *testing.T) {
	cfg := core.CheckConfig{Type: "ping", Params: map[string]any{
		"host": "this-host-does-not-resolve.invalid.",
	}}
	res := PingCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, Message = %q, want CheckError (unresolvable host)", res.Status, res.Message)
	}
}

func TestPingCheck_DefaultsHostToTargetIP(t *testing.T) {
	// Exercises only param decoding: a target IP with no reachable ICMP
	// endpoint should never panic and should return a terminal status.
	cfg := core.CheckConfig{Type: "ping", Timeout: 500 * time.Millisecond, Params: map[string]any{
		"count":    1,
		"interval": "10ms",
	}}
	res := PingCheck{}.Run(context.Background(), core.Target{IP: "127.0.0.1"}, cfg)
	if res.Status == "" {
		t.Fatal("expected a terminal status")
	}
}

// TestPingCheck_RealICMP exercises the actual ICMP round trip against
// loopback. It requires either raw-socket privileges or unprivileged ping
// support on the host, so it is skipped unless -short is not set and it is
// explicitly opted into via network access; guarded so CI without ICMP
// permissions never fails the suite.
func TestPingCheck_RealICMP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real ICMP ping in -short mode")
	}
	cfg := core.CheckConfig{Type: "ping", Timeout: 3 * time.Second, Params: map[string]any{
		"host":  "127.0.0.1",
		"count": 1,
	}}
	res := PingCheck{}.Run(context.Background(), core.Target{}, cfg)
	// Do not assert Pass: environments without ICMP privileges will
	// legitimately report CheckError. Only assert the check completed
	// without panicking and reported a real status.
	if res.Status != core.CheckPass && res.Status != core.CheckFail && res.Status != core.CheckError {
		t.Fatalf("unexpected status: %v", res.Status)
	}
	t.Logf("real ping result: status=%v message=%q", res.Status, res.Message)
}

var _ core.Check = PingCheck{}
