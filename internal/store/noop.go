package store

import (
	"context"

	"github.com/restorelab/restorelab/internal/core"
)

// Noop satisfies Store by doing nothing at all.
//
// It is wired whenever there is no usable database: none configured, the file
// unreadable, the server unreachable, the schema behind. Callers therefore
// never carry an "if store != nil" branch, and the guarantee that a missing
// database changes nothing is testable, because the same code path runs in
// both cases.
type Noop struct{}

var _ Store = Noop{}

func (Noop) CreateRun(context.Context, *core.RecoveryRun, string) error     { return nil }
func (Noop) UpdateRun(context.Context, *core.RecoveryRun) error             { return nil }
func (Noop) SaveStep(context.Context, string, int, core.Step) error         { return nil }
func (Noop) SaveCheck(context.Context, string, int, core.CheckResult) error { return nil }
func (Noop) AppendEvent(context.Context, string, Event) error               { return nil }
func (Noop) ListRuns(context.Context, Filter) ([]RunSummary, error)         { return nil, nil }
func (Noop) Events(context.Context, string, int64) ([]Event, error)         { return nil, nil }
func (Noop) Describe() string                                               { return "no database (history is not being recorded)" }
func (Noop) Close() error                                                   { return nil }

// GetRun reports ErrNotFound rather than nil: a caller asking for a specific
// run must be told it does not have it, not handed an empty one.
func (Noop) GetRun(context.Context, string) (*core.RecoveryRun, error) {
	return nil, ErrNotFound
}
