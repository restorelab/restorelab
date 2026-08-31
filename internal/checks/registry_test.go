package checks

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// fakeCheck is a scriptable core.Check for exercising the registry without
// touching the network.
type fakeCheck struct {
	typ string
	run func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult
}

func (f *fakeCheck) Type() string { return f.typ }
func (f *fakeCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	return f.run(ctx, target, cfg)
}

func TestRegistry_GetRegisterTypes(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "zeta"})
	r.Register(&fakeCheck{typ: "alpha"})

	if _, ok := r.Get("alpha"); !ok {
		t.Fatal("expected alpha to be registered")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("did not expect missing to be registered")
	}

	types := r.Types()
	if len(types) != 2 || types[0] != "alpha" || types[1] != "zeta" {
		t.Fatalf("Types() = %v, want sorted [alpha zeta]", types)
	}
}

func TestDefault_RegistersBuiltins(t *testing.T) {
	r := Default()
	want := []string{"dns", "http", "https", "ping", "tcp"}
	got := r.Types()
	if len(got) != len(want) {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Types() = %v, want %v", got, want)
		}
	}
}

func TestRunOne_UnknownType(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "known"})

	res := r.RunOne(context.Background(), core.Target{}, core.CheckConfig{Type: "bogus", Name: "n"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if !strings.Contains(res.Message, "bogus") || !strings.Contains(res.Message, "known") {
		t.Fatalf("Message should list the bad type and known types, got: %q", res.Message)
	}
	if res.Name != "n" || res.Type != "bogus" {
		t.Fatalf("Name/Type not filled: %+v", res)
	}
}

func TestRunOne_RetriesUntilPass(t *testing.T) {
	var calls int32
	r := New()
	r.Register(&fakeCheck{typ: "flaky", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return core.CheckResult{Status: core.CheckFail, Message: "not yet"}
		}
		return core.CheckResult{Status: core.CheckPass, Message: "ok"}
	}})

	cfg := core.CheckConfig{Type: "flaky", Retries: 5, RetryInterval: time.Millisecond}
	res := r.RunOne(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, want CheckPass", res.Status)
	}
	if res.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", res.Attempts)
	}
}

func TestRunOne_AlwaysFails_AttemptsIsRetriesPlusOne(t *testing.T) {
	var calls int32
	r := New()
	r.Register(&fakeCheck{typ: "broken", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		atomic.AddInt32(&calls, 1)
		return core.CheckResult{Status: core.CheckFail, Message: "nope"}
	}})

	cfg := core.CheckConfig{Type: "broken", Retries: 2, RetryInterval: time.Millisecond}
	res := r.RunOne(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
	if res.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3 (retries+1)", res.Attempts)
	}
	if int(calls) != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRunOne_RetryIntervalRespected(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "broken", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		return core.CheckResult{Status: core.CheckFail}
	}})

	interval := 30 * time.Millisecond
	cfg := core.CheckConfig{Type: "broken", Retries: 2, RetryInterval: interval}
	start := time.Now()
	res := r.RunOne(context.Background(), core.Target{}, cfg)
	elapsed := time.Since(start)

	if res.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", res.Attempts)
	}
	// Two waits of `interval` between three attempts.
	minExpected := 2 * interval
	if elapsed < minExpected {
		t.Fatalf("elapsed %v, want at least %v (retry interval not respected)", elapsed, minExpected)
	}
	if res.Duration < minExpected {
		t.Fatalf("Duration %v, want at least %v", res.Duration, minExpected)
	}
}

func TestRunOne_PerAttemptTimeout(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "slow", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		<-ctx.Done()
		return core.CheckResult{Status: core.CheckError, Message: "context: " + ctx.Err().Error()}
	}})

	cfg := core.CheckConfig{Type: "slow", Timeout: 20 * time.Millisecond}
	start := time.Now()
	res := r.RunOne(context.Background(), core.Target{}, cfg)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("check should have been bounded by cfg.Timeout, took %v", elapsed)
	}
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
}

func TestRunOne_PanicRecovered(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "panicky", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		panic("boom")
	}})

	res := r.RunOne(context.Background(), core.Target{}, core.CheckConfig{Type: "panicky"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if !strings.Contains(res.Message, "panicky") || !strings.Contains(res.Message, "boom") {
		t.Fatalf("Message should name the check and the panic value, got: %q", res.Message)
	}
}

func TestRunAll_CancelledContext_SkipsRemainder(t *testing.T) {
	var calls int32
	r := New()
	r.Register(&fakeCheck{typ: "counted", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		atomic.AddInt32(&calls, 1)
		return core.CheckResult{Status: core.CheckPass}
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front so nothing runs

	cfgs := []core.CheckConfig{
		{Type: "counted", Name: "c1"},
		{Type: "counted", Name: "c2"},
		{Type: "counted", Name: "c3"},
	}
	results := r.RunAll(ctx, core.Target{}, cfgs)

	if len(results) != len(cfgs) {
		t.Fatalf("RunAll returned %d results, want %d (every configured check must be accounted for)", len(results), len(cfgs))
	}
	for i, res := range results {
		if res.Status != core.CheckSkipped {
			t.Fatalf("result[%d].Status = %v, want CheckSkipped", i, res.Status)
		}
		if !strings.Contains(res.Message, "cancelled") {
			t.Fatalf("result[%d].Message = %q, want it to mention cancellation", i, res.Message)
		}
		if res.Name != cfgs[i].Name {
			t.Fatalf("result[%d].Name = %q, want %q", i, res.Name, cfgs[i].Name)
		}
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 (nothing should have run)", calls)
	}
}

func TestRunAll_PartialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New()
	r.Register(&fakeCheck{typ: "cancels-after-first", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		cancel() // cancel once the first check has run
		return core.CheckResult{Status: core.CheckPass}
	}})

	cfgs := []core.CheckConfig{
		{Type: "cancels-after-first", Name: "c1"},
		{Type: "cancels-after-first", Name: "c2"},
	}
	results := r.RunAll(ctx, core.Target{}, cfgs)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Status != core.CheckPass {
		t.Fatalf("result[0].Status = %v, want CheckPass", results[0].Status)
	}
	if results[1].Status != core.CheckSkipped {
		t.Fatalf("result[1].Status = %v, want CheckSkipped", results[1].Status)
	}
}

func TestRunOne_FillsNameTypeDurationAttempts(t *testing.T) {
	r := New()
	r.Register(&fakeCheck{typ: "ok", run: func(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
		time.Sleep(5 * time.Millisecond)
		return core.CheckResult{Status: core.CheckPass}
	}})

	res := r.RunOne(context.Background(), core.Target{}, core.CheckConfig{Type: "ok", Name: "my-check"})
	if res.Name != "my-check" || res.Type != "ok" {
		t.Fatalf("Name/Type = %q/%q, want my-check/ok", res.Name, res.Type)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", res.Attempts)
	}
	if res.StartedAt.IsZero() || res.CompletedAt.IsZero() {
		t.Fatal("StartedAt/CompletedAt should be set")
	}
	if res.Duration <= 0 {
		t.Fatal("Duration should be positive")
	}
	if !res.CompletedAt.After(res.StartedAt) && !res.CompletedAt.Equal(res.StartedAt) {
		t.Fatal("CompletedAt should not be before StartedAt")
	}
}

var _ core.Check = (*fakeCheck)(nil)
