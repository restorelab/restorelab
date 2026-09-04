// Package notify decides when RestoreLab has something worth telling a human,
// and delivers it to Discord, Slack or a generic webhook.
//
// The hard part is not the delivery, it is the silence. A scheduler drilling
// twenty workloads every night produces twenty green messages every night,
// the channel is muted within a week, and the one red message that mattered
// is never read. So this package speaks only when what the product proves
// about a workload has changed, and Decide is where that judgement lives.
package notify

import (
	"fmt"

	"github.com/restorelab/restorelab/internal/core"
)

// Story is what the product currently asserts about a workload: how its last
// drill graded, and what that drill established.
//
// The two travel together because either one moving alone is news. A workload
// that stays SUCCESS while falling from DATA to SERVICE has regressed, and
// nothing else in the product would say so.
type Story struct {
	Result     core.RunResult
	ProofLevel core.ProofLevel
}

// Kind names why a transition is worth a message. It is part of the webhook
// payload, so these strings are a public interface: renaming one breaks
// somebody's filter.
type Kind string

const (
	// KindFirstVerdict is a workload's first run that reached a verdict.
	// There is nothing to compare against, but a baseline being set is
	// genuine news to whoever just configured the plan.
	KindFirstVerdict Kind = "first_verdict"

	// KindVerdict is a changed verdict, in either direction. Recovery is
	// reported as loudly as breakage: "you can stop worrying" is worth as
	// much as "start worrying", and a channel that only ever brings bad news
	// gets muted for that reason alone.
	KindVerdict Kind = "verdict_changed"

	// KindProofDropped is a fall down the proof ladder at a constant
	// verdict. This is the transition E1 built the ladder to make visible.
	KindProofDropped Kind = "proof_dropped"

	// KindUnevaluable is the first run that reached no verdict after one
	// that did. A workload whose drills cannot be evaluated is not being
	// verified at all, and staying silent about it would be the same failure
	// C5 was built against, one level up.
	KindUnevaluable Kind = "became_unevaluable"

	// KindEvaluable is the return: a run that reached a verdict after one
	// that could not. It fires whatever that verdict is, because being able
	// to see the workload again is the news.
	KindEvaluable Kind = "became_evaluable_again"
)

// Transition is one thing worth saying about one workload.
type Transition struct {
	Kind     Kind
	Current  Story
	Previous *Story
	Headline string
}

// Decide reports whether a terminal run is worth a message, and why.
//
// state is the run's terminal state, current what it established, previous
// the story of this workload's most recent earlier run that reached a
// verdict (nil when there is none), and previousUnevaluable whether the run
// immediately before this one reached no verdict.
//
// previous and previousUnevaluable deliberately look at two different runs.
// previous skips over verdict-less runs, because a workload with an
// impeccable history must not appear to collapse just because its latest
// drill could not be evaluated; that is the same rule the confidence ceiling
// and the failure rate already follow. previousUnevaluable looks at the
// immediately preceding run, because "we could not see this workload, and now
// we can" is a fact about consecutive attempts.
func Decide(state core.RunState, current Story, previous *Story, previousUnevaluable bool) (Transition, bool) {
	if !state.Terminal() {
		return Transition{}, false
	}

	// A cancelled run proves nothing and surprises nobody: a human stopped
	// it, and that human already knows. Same reasoning as markCancelled.
	if state == core.RunCancelled {
		return Transition{}, false
	}

	t := Transition{Current: current, Previous: previous}

	if state == core.RunInconclusive {
		// Consecutive inconclusive runs are one situation, not a stream of
		// events. Only the edge into it is news.
		if previousUnevaluable {
			return Transition{}, false
		}
		if previous == nil {
			// A workload whose very first drill could not be evaluated has
			// no baseline to lose, but somebody just configured a plan that
			// does not work. That is worth exactly one message.
			t.Kind, t.Headline = KindUnevaluable, "first drill could not be evaluated"
			return t, true
		}
		t.Kind = KindUnevaluable
		t.Headline = fmt.Sprintf("no longer evaluable, was %s", previous.Result)
		return t, true
	}

	if previous == nil {
		t.Kind = KindFirstVerdict
		t.Headline = fmt.Sprintf("first verdict: %s", current.Result)
		return t, true
	}

	if current.Result != previous.Result {
		t.Kind = KindVerdict
		t.Headline = fmt.Sprintf("%s, was %s", current.Result, previous.Result)
		return t, true
	}

	// Coming back from a run nobody could evaluate is news whatever the
	// verdict is, and it is checked before the proof ladder so that the
	// message says the more important of the two things.
	if previousUnevaluable {
		t.Kind = KindEvaluable
		t.Headline = fmt.Sprintf("evaluable again: %s", current.Result)
		return t, true
	}

	// A level is only compared when both ends were actually recorded.
	// core.ProofUnknown means "not written down", never "nothing proven", so
	// treating it as a floor would invent a regression out of a run that
	// predates the column. That is the exact mistake E1 refused to make when
	// it declined to backfill.
	if current.ProofLevel.Recorded() && previous.ProofLevel.Recorded() &&
		current.ProofLevel.Rank() < previous.ProofLevel.Rank() {
		t.Kind = KindProofDropped
		t.Headline = fmt.Sprintf("proof dropped from %s to %s", previous.ProofLevel, current.ProofLevel)
		return t, true
	}

	return Transition{}, false
}
