package store

import (
	"context"
	"errors"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

func TestNoopNeverErrorsOnWrites(t *testing.T) {
	ctx := context.Background()
	var s Store = Noop{}
	run := &core.RecoveryRun{ID: "abc"}

	for name, err := range map[string]error{
		"CreateRun":   s.CreateRun(ctx, run, "name: x"),
		"UpdateRun":   s.UpdateRun(ctx, run),
		"SaveStep":    s.SaveStep(ctx, "abc", 0, core.Step{Name: "restore"}),
		"SaveCheck":   s.SaveCheck(ctx, "abc", 0, core.CheckResult{Name: "tcp"}),
		"AppendEvent": s.AppendEvent(ctx, "abc", Event{Seq: 1}),
		"Close":       s.Close(),
	} {
		if err != nil {
			t.Errorf("%s returned %v, want nil: a write must never fail a drill", name, err)
		}
	}
}

func TestNoopGetRunReportsNotFound(t *testing.T) {
	_, err := Noop{}.GetRun(context.Background(), "abc")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun error = %v, want ErrNotFound", err)
	}
}

// A catalogue without a database must refuse, not pretend. The whole point of
// `plan apply` is that the plan is somewhere the API and the scheduler can
// find it later; a silent success would hand the operator a plan that exists
// nowhere.
func TestNoopPlansRefuse(t *testing.T) {
	ctx := context.Background()
	var n Noop
	if err := n.CreatePlan(ctx, Plan{}); !errors.Is(err, ErrNoHistory) {
		t.Errorf("CreatePlan = %v, want ErrNoHistory", err)
	}
	if err := n.UpdatePlan(ctx, Plan{}, 0); !errors.Is(err, ErrNoHistory) {
		t.Errorf("UpdatePlan = %v, want ErrNoHistory", err)
	}
	if _, err := n.GetPlan(ctx, "x"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("GetPlan = %v, want ErrNoHistory", err)
	}
	if _, err := n.ListPlans(ctx, PlanFilter{}); !errors.Is(err, ErrNoHistory) {
		t.Errorf("ListPlans = %v, want ErrNoHistory", err)
	}
	if err := n.DeletePlan(ctx, "x"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("DeletePlan = %v, want ErrNoHistory", err)
	}
}

func TestNoopDescribeMentionsNoDatabase(t *testing.T) {
	if got := (Noop{}).Describe(); got == "" {
		t.Fatal("Describe must say something a user can read")
	}
}
