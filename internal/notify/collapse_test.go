package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

func TestAValueThatFellToZeroIsWorthAMessage(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	tr, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"orders": 0},
		map[string]float64{"orders": 1204331})

	if !said {
		t.Fatal("a value that fell to zero produced no message")
	}
	if tr.Kind != KindValueCollapsed {
		t.Errorf("kind = %q, want %q", tr.Kind, KindValueCollapsed)
	}
	if !strings.Contains(tr.Headline, "orders") {
		t.Errorf("headline = %q, want the name of the value in it", tr.Headline)
	}
	if !strings.Contains(tr.Headline, "1204331") {
		t.Errorf("headline = %q, want what the value used to be in it, in full", tr.Headline)
	}
	if strings.Contains(tr.Headline, "e+") {
		t.Errorf("headline = %q, want a number a human can read", tr.Headline)
	}
}

// The restraint is the feature, and this is the test that holds it.
//
// A value that halved is undeclared drift: the plan set no tolerance, so
// nobody agreed on a threshold, and RestoreLab is not going to invent one.
// A table purged on Friday is not a failed recovery, and a channel that says
// it is gets muted before the night it matters.
func TestAValueThatMerelyHalvedSaysNothing(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	_, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"orders": 602165},
		map[string]float64{"orders": 1204331})

	if said {
		t.Error("a value that halved with no declared tolerance produced a message")
	}
}

func TestAValueThatWasAlreadyZeroSaysNothing(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	_, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"orders": 0},
		map[string]float64{"orders": 0})

	if said {
		t.Error("a value that was already zero produced a message: the collapse is the news, not the state")
	}
}

// A first drill has nothing to have fallen from. Announcing an empty table on
// the first night would make every new workload arrive with an alarm.
func TestAValueWithNoEarlierReadingSaysNothing(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	_, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"orders": 0}, nil)

	if said {
		t.Error("a value nobody has measured before produced a message")
	}
}

// A run nobody could evaluate is not evidence about the workload in either
// direction. It is the same rule the drift window follows when it skips
// verdict-less runs, and the same one the confidence ceiling follows.
func TestARunThatReachedNoVerdictSaysNothingAboutItsValues(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	for _, state := range []core.RunState{core.RunCancelled, core.RunInconclusive} {
		_, said := DecideCollapse(state, Story{}, previous,
			map[string]float64{"orders": 0},
			map[string]float64{"orders": 1204331})
		if said {
			t.Errorf("a %s run produced a message about a value", state)
		}
	}
}

func TestARunStillGoingSaysNothingAboutItsValues(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	_, said := DecideCollapse(core.RunRunningChecks, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"orders": 0},
		map[string]float64{"orders": 1204331})

	if said {
		t.Error("a run that has not finished produced a message about a value")
	}
}

// Two values collapsing in one drill is one message, and which one it names
// must not depend on Go's map ordering: a message that differs between two
// runs of the same input is a message nobody can reproduce.
func TestTwoCollapsedValuesNameTheFirstByName(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	for range 20 {
		tr, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
			map[string]float64{"orders": 0, "customers": 0},
			map[string]float64{"orders": 1204331, "customers": 88})
		if !said {
			t.Fatal("two collapsed values produced no message")
		}
		if !strings.Contains(tr.Headline, "customers") {
			t.Fatalf("headline = %q, want the first value by name", tr.Headline)
		}
	}
}

// A negative reading reaching zero went up, and a rise is never a collapse.
// It is the same rule CheckDrift follows: max_drop is a floor, not a band.
func TestAValueThatRoseToZeroSaysNothing(t *testing.T) {
	previous := story(core.ResultSuccess, core.ProofData)
	_, said := DecideCollapse(core.RunSuccess, *story(core.ResultSuccess, core.ProofData), previous,
		map[string]float64{"lag": 0},
		map[string]float64{"lag": -12})

	if said {
		t.Error("a value that rose to zero produced a message")
	}
}

func TestZeroedIsWhatDecidesWhetherThePastIsWorthReading(t *testing.T) {
	if Zeroed(nil) {
		t.Error("a drill that measured nothing looks worth a second query")
	}
	if Zeroed(map[string]float64{"orders": 1204331}) {
		t.Error("a drill whose values are all non-zero looks worth a second query")
	}
	if !Zeroed(map[string]float64{"orders": 1204331, "sessions": 0}) {
		t.Error("a drill with a value at zero does not look worth a second query")
	}
}

// The values of one run arrive keyed by check, and a collapse is compared
// across runs, where the check numbering may have moved. The capture name is
// what identifies a measurement to a human, so that is the key.
func TestValuesByNameFlattensACheckIndexedMap(t *testing.T) {
	got := valuesByName(map[int]map[string]float64{
		0: {"orders": 1204331},
		2: {"sessions": 88},
	})
	if len(got) != 2 || got["orders"] != 1204331 || got["sessions"] != 88 {
		t.Errorf("values = %#v, want orders and sessions", got)
	}
}

// Two checks capturing the same name in one drill make that name ambiguous
// across runs, and guessing which of the two collapsed is how a message names
// the wrong table. Refusing to guess costs a notification nobody could have
// trusted.
func TestValuesByNameDropsANameTwoChecksMeasured(t *testing.T) {
	got := valuesByName(map[int]map[string]float64{
		0: {"rows": 1204331},
		1: {"rows": 0},
	})
	if _, ok := got["rows"]; ok {
		t.Errorf("values = %#v, want the ambiguous name dropped", got)
	}
}

// --- the dispatcher ----------------------------------------------------------

func TestACollapsedValueReachesAChannel(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultSuccess, core.ProofData)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofData)
	f.previous = &before
	f.values = map[string]map[int]map[string]float64{
		"run-1": {0: {"orders": 0}},
		"run-0": {0: {"orders": 1204331}},
	}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	rows := f.createdRows()
	if len(rows) != 1 {
		t.Fatalf("got %d deliveries, want one: %+v", len(rows), rows)
	}
	if rows[0].Kind != string(KindValueCollapsed) {
		t.Errorf("kind = %q, want %q", rows[0].Kind, KindValueCollapsed)
	}
	if !strings.Contains(rows[0].Payload, "orders") {
		t.Errorf("payload does not name the value: %s", rows[0].Payload)
	}
}

// The verdict is the bigger fact, and one run can only produce one message
// per channel. A collapse is what is left to say when nothing else moved.
func TestAChangedVerdictStillWinsOverACollapsedValue(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofData)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofData)
	f.previous = &before
	f.values = map[string]map[int]map[string]float64{
		"run-1": {0: {"orders": 0}},
		"run-0": {0: {"orders": 1204331}},
	}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	rows := f.createdRows()
	if len(rows) != 1 {
		t.Fatalf("got %d deliveries, want one: %+v", len(rows), rows)
	}
	if rows[0].Kind != string(KindVerdict) {
		t.Errorf("kind = %q, want %q: the verdict moved and that is the news", rows[0].Kind, KindVerdict)
	}
}

// The perf contract, stated as a test. A run the dispatcher did not claim
// costs no query at all: another dispatcher owns it, and reading its values
// would be one round trip per run per tick for work somebody else is doing.
func TestARunNobodyClaimedIsNeverReadForValues(t *testing.T) {
	key := testKey(t)

	f := newFakeStore()
	f.claimed = false
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultSuccess, core.ProofData)}

	d, _ := newTestDispatcher(t, f, key)
	d.Tick(context.Background())

	if n := f.valueCalls(); n != 0 {
		t.Errorf("%d value read(s) for a run this dispatcher did not claim, want 0", n)
	}
}

// The second half of the perf contract: the previous drill is read only when
// this one holds a value at zero. Nothing else can be a collapse, so nothing
// else is worth a round trip.
func TestThePreviousDrillIsReadOnlyWhenSomethingIsAtZero(t *testing.T) {
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultSuccess, core.ProofData)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofData)
	f.previous = &before
	f.values = map[string]map[int]map[string]float64{
		"run-1": {0: {"orders": 1206890}},
		"run-0": {0: {"orders": 1204331}},
	}

	d, _ := newTestDispatcher(t, f, key)
	d.Tick(context.Background())

	if n := f.valueCalls(); n != 1 {
		t.Errorf("%d value read(s), want exactly the claimed run's own", n)
	}
}
