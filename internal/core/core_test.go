package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPrimaryIPPrefersIPv4(t *testing.T) {
	tests := []struct {
		name string
		ips  []string
		want string
	}{
		{name: "none", ips: nil, want: ""},
		{name: "single ipv4", ips: []string{"10.99.0.14"}, want: "10.99.0.14"},
		{name: "ipv6 first", ips: []string{"fd00::1", "10.99.0.14"}, want: "10.99.0.14"},
		{name: "only ipv6", ips: []string{"fd00::1"}, want: "fd00::1"},
		{name: "first ipv4 wins", ips: []string{"10.99.0.14", "10.99.0.15"}, want: "10.99.0.14"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := WorkloadStatus{IPs: tt.ips}
			if got := s.PrimaryIP(); got != tt.want {
				t.Errorf("PrimaryIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunStateTerminal(t *testing.T) {
	terminal := []RunState{RunSuccess, RunFailed, RunCancelled, RunCleanupFailed}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	running := []RunState{RunQueued, RunDiscoveringBackup, RunRestoring, RunRunningChecks, RunCleaningUp}
	for _, s := range running {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestRTOExceeded(t *testing.T) {
	tests := []struct {
		name   string
		rto    time.Duration
		target time.Duration
		want   bool
	}{
		{name: "no target set", rto: time.Hour, target: 0, want: false},
		{name: "under target", rto: 2 * time.Minute, target: 5 * time.Minute, want: false},
		{name: "exactly on target", rto: 5 * time.Minute, target: 5 * time.Minute, want: false},
		{name: "over target", rto: 7 * time.Minute, target: 5 * time.Minute, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := RecoveryRun{RTO: tt.rto, RTOTarget: tt.target}
			if got := r.RTOExceeded(); got != tt.want {
				t.Errorf("RTOExceeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStepDurationAndFailedChecks(t *testing.T) {
	run := RecoveryRun{
		Steps: []Step{
			{Name: "restore", Duration: 84 * time.Second},
			{Name: "start", Duration: 2 * time.Second},
		},
		Checks: []CheckResult{
			{Name: "TCP 22", Status: CheckPass},
			{Name: "HTTP health", Status: CheckFail},
			{Name: "ping", Status: CheckError},
			{Name: "postgres", Status: CheckSkipped},
		},
	}

	if got := run.StepDuration("restore"); got != 84*time.Second {
		t.Errorf("StepDuration(restore) = %v", got)
	}
	if got := run.StepDuration("nope"); got != 0 {
		t.Errorf("StepDuration for an unknown step = %v, want 0", got)
	}
	if got := len(run.FailedChecks()); got != 3 {
		t.Errorf("len(FailedChecks()) = %d, want 3 (fail, error and skipped all count as not passing)", got)
	}
}

func TestRetryableWrapping(t *testing.T) {
	base := errors.New("connection reset")

	if Retryable(nil) != nil {
		t.Error("Retryable(nil) must stay nil")
	}
	if IsRetryable(base) {
		t.Error("a plain error must not be retryable")
	}

	wrapped := Retryable(base)
	if !IsRetryable(wrapped) {
		t.Error("wrapped error should be retryable")
	}
	if !errors.Is(wrapped, base) {
		t.Error("wrapping must preserve errors.Is on the cause")
	}
	if wrapped.Error() != base.Error() {
		t.Errorf("Error() = %q, want the cause's message", wrapped.Error())
	}

	// Retryability must survive further wrapping, which is how providers and
	// the engine hand errors up the stack.
	deep := fmt.Errorf("restore vm 9101: %w", wrapped)
	if !IsRetryable(deep) {
		t.Error("retryability must survive %w wrapping")
	}
	if !errors.Is(deep, base) {
		t.Error("errors.Is must still reach the cause through both wrappers")
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := map[string]error{
		"not found":    ErrNotFound,
		"no backup":    ErrNoBackup,
		"not managed":  ErrNotManaged,
		"not isolated": ErrNetworkNotIsolated,
		"unauthorized": ErrUnauthorized,
		"no capacity":  ErrInsufficientCapacity,
		"unsupported":  ErrUnsupported,
		"timeout":      ErrTimeout,
		"cancelled":    ErrCancelled,
	}
	for name, err := range all {
		for otherName, other := range all {
			if name == otherName {
				continue
			}
			if errors.Is(err, other) {
				t.Errorf("%q and %q must be distinct sentinels", name, otherName)
			}
		}
	}
}

func TestTargetTemplateVars(t *testing.T) {
	target := Target{
		IP:         "10.99.0.14",
		WorkloadID: "9101",
		Node:       "pve02",
		Name:       "postgres-prod",
		Vars:       map[string]string{"port": "5432", "ip": "overridden"},
	}

	vars := target.TemplateVars()
	if vars["id"] != "9101" || vars["node"] != "pve02" || vars["name"] != "postgres-prod" {
		t.Errorf("TemplateVars() = %v", vars)
	}
	if vars["target"] != "10.99.0.14" {
		t.Errorf("target alias = %q", vars["target"])
	}
	if vars["port"] != "5432" {
		t.Errorf("custom vars must be exposed, got %v", vars)
	}
	// Explicit user-supplied vars win: a plan author asking for a specific
	// value must not be silently overridden by the discovered one.
	if vars["ip"] != "overridden" {
		t.Errorf("ip = %q, want the explicit override", vars["ip"])
	}
}

func TestMemoryFreeBytesNeverNegative(t *testing.T) {
	n := Node{MemoryTotalBytes: 8 << 30, MemoryUsedBytes: 10 << 30}
	if got := n.MemoryFreeBytes(); got != 0 {
		t.Errorf("MemoryFreeBytes() = %d, want 0 when a provider over-reports usage", got)
	}
	n = Node{MemoryTotalBytes: 8 << 30, MemoryUsedBytes: 2 << 30}
	if got, want := n.MemoryFreeBytes(), int64(6<<30); got != want {
		t.Errorf("MemoryFreeBytes() = %d, want %d", got, want)
	}
}

func TestBackupAge(t *testing.T) {
	b := Backup{CreatedAt: time.Now().Add(-2 * time.Hour)}
	if age := b.Age(); age < 2*time.Hour || age > 2*time.Hour+time.Minute {
		t.Errorf("Age() = %v, want ~2h", age)
	}
}

func TestCheckResultOK(t *testing.T) {
	for status, want := range map[CheckStatus]bool{
		CheckPass:    true,
		CheckFail:    false,
		CheckError:   false,
		CheckSkipped: false,
	} {
		if got := (CheckResult{Status: status}).OK(); got != want {
			t.Errorf("OK() for %s = %v, want %v", status, got, want)
		}
	}
}
