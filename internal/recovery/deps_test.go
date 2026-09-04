package recovery

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// The recovery engine holds no database handle, and this test is the thing
// that keeps that true.
//
// The rule is not stylistic. A drill is destructive work on a production
// cluster; the journal writes the history, the engine emits events, and that
// separation is what stops a locked or corrupt database from being able to
// fail a drill. The moment Deps carries a store.Store, every method on it
// becomes reachable from inside the workflow and the separation is gone by
// accident rather than by decision.
//
// It is written with reflection rather than by importing internal/store,
// because importing the package here would be the very thing under test. A
// field whose type comes from internal/store is what a reader would have to
// justify, so this makes them justify it here.
func TestDepsHoldNoStore(t *testing.T) {
	deps := reflect.TypeOf(Deps{})
	for i := 0; i < deps.NumField(); i++ {
		f := deps.Field(i)
		if pkg := typePackage(f.Type); strings.HasSuffix(pkg, "/internal/store") {
			t.Errorf("Deps.%s has type %s, from the store package: the engine must receive "+
				"read-only capabilities, never a database handle", f.Name, f.Type)
		}
	}
}

// Baselines is one read-only method and stays one. Widening it to anything
// with a Save on it would put a write inside the workflow.
func TestDepsBaselinesIsTheReadOnlyInterface(t *testing.T) {
	f, ok := reflect.TypeOf(Deps{}).FieldByName("Baselines")
	if !ok {
		t.Fatal("Deps has no Baselines field")
	}
	want := reflect.TypeOf((*core.BaselineReader)(nil)).Elem()
	if f.Type != want {
		t.Fatalf("Deps.Baselines is %s, want %s", f.Type, want)
	}
	if n := want.NumMethod(); n != 1 {
		t.Fatalf("core.BaselineReader has %d methods, want exactly 1: it is a capability, not a store", n)
	}
}

// typePackage names the package a type was declared in, unwrapping the one
// level of indirection a field can add. A named type in another package is
// what matters; an anonymous struct or a builtin has no package at all.
func typePackage(t reflect.Type) string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	return t.PkgPath()
}

// nopBaselines is a core.BaselineReader that answers nothing. It exists to be
// recognised by identity on the far side of the workflow.
type nopBaselines struct{}

func (nopBaselines) Values(context.Context, string, string, int) ([]float64, error) {
	return nil, nil
}

// The reader has to reach the checks, and it has to reach them on both paths
// that build a target. The startup-skipped path is the one that is easy to
// forget: it returns a target of its own a hundred lines away from the other,
// and a check running there would silently report every drift comparison as
// having no history.
func TestBaselinesReachTheChecksTarget(t *testing.T) {
	tests := []struct {
		name string
		skip bool
	}{
		{"guest booted", false},
		{"startup skipped", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hv := &fakeProvider{
				idStr:        "fake-hv",
				latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "101", CreatedAt: time.Now().Add(-time.Hour)},
			}
			hv.statuses = []core.WorkloadStatus{
				{PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.99.0.14"}},
			}

			var gotTarget core.Target
			runner := &fakeCheckRunner{
				fn: func(_ context.Context, target core.Target, _ []core.CheckConfig) []core.CheckResult {
					gotTarget = target
					return []core.CheckResult{{Name: "service", Type: "command", Status: core.CheckPass}}
				},
			}

			want := nopBaselines{}
			clock := newFakeClock()
			engine, err := New(Deps{
				Hypervisor: hv,
				Backups:    hv,
				Checks:     runner,
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				Now:        clock.Now,
				Sleep:      fakeSleepFn(clock),
				Baselines:  want,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			p := &plan.Plan{
				Name:     "baselines",
				Workload: plan.WorkloadRef{Provider: "fake", ID: "101"},
				Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest},
				Startup:  plan.StartupSpec{Timeout: plan.Duration(time.Minute), Skip: tt.skip},
				Checks:   []plan.CheckSpec{{Type: "command", Params: map[string]any{"run": "q"}}},
			}
			p.ApplyDefaults()

			if _, err := engine.Run(context.Background(), p, RunOptions{
				Network: isolatedNetwork(),
				Node:    "pve1",
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if gotTarget.Baseline != core.BaselineReader(want) {
				t.Fatalf("Target.Baseline = %#v, want the reader given to Deps", gotTarget.Baseline)
			}
		})
	}
}

// A drill built with no reader hands the checks a nil one, which is what a
// drift check reads as "there is no history" rather than as a failure. An
// engine that substituted an empty stand-in here would make a real history
// that happens to be empty indistinguishable from no history at all.
func TestNoBaselinesLeavesTargetBaselineNil(t *testing.T) {
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "101", CreatedAt: time.Now().Add(-time.Hour)},
	}
	hv.statuses = []core.WorkloadStatus{
		{PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.99.0.14"}},
	}

	var gotTarget core.Target
	runner := &fakeCheckRunner{
		fn: func(_ context.Context, target core.Target, _ []core.CheckConfig) []core.CheckResult {
			gotTarget = target
			return []core.CheckResult{{Name: "service", Type: "command", Status: core.CheckPass}}
		},
	}

	engine := newTestEngine(t, hv, hv, runner, newFakeClock())
	p := &plan.Plan{
		Name:     "no-baselines",
		Workload: plan.WorkloadRef{Provider: "fake", ID: "101"},
		Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest},
		Startup:  plan.StartupSpec{Timeout: plan.Duration(time.Minute)},
		Checks:   []plan.CheckSpec{{Type: "command", Params: map[string]any{"run": "q"}}},
	}
	p.ApplyDefaults()

	if _, err := engine.Run(context.Background(), p, RunOptions{
		Network: isolatedNetwork(),
		Node:    "pve1",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotTarget.Baseline != nil {
		t.Fatalf("Target.Baseline = %#v, want nil", gotTarget.Baseline)
	}
}
