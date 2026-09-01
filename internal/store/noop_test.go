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

func TestNoopDescribeMentionsNoDatabase(t *testing.T) {
	if got := (Noop{}).Describe(); got == "" {
		t.Fatal("Describe must say something a user can read")
	}
}
