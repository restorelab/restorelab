package notify

import (
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

func story(r core.RunResult, l core.ProofLevel) *Story {
	return &Story{Result: r, ProofLevel: l}
}

// TestDecide is the feature. Every row is a claim about when this product
// has something worth saying, and the silent rows matter as much as the
// loud ones: a channel that speaks every night is a channel nobody reads.
func TestDecide(t *testing.T) {
	cases := []struct {
		name string

		state          core.RunState
		current        Story
		previous       *Story
		wasUnevaluable bool

		want     Kind
		wantSaid bool
	}{
		{
			name:     "first run of a workload sets the baseline",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofService},
			previous: nil,
			want:     KindFirstVerdict, wantSaid: true,
		},
		{
			name:     "green stays green and says nothing",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofService},
			previous: story(core.ResultSuccess, core.ProofService),
			wantSaid: false,
		},
		{
			name:     "success becomes failure",
			state:    core.RunFailed,
			current:  Story{core.ResultFailed, core.ProofBoot},
			previous: story(core.ResultSuccess, core.ProofService),
			want:     KindVerdict, wantSaid: true,
		},
		{
			name:     "failure becomes success, and that is worth as much",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofService},
			previous: story(core.ResultFailed, core.ProofBoot),
			want:     KindVerdict, wantSaid: true,
		},
		{
			name:     "still green but proving less",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofService},
			previous: story(core.ResultSuccess, core.ProofData),
			want:     KindProofDropped, wantSaid: true,
		},
		{
			name:     "still green and proving more says nothing on its own",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofData},
			previous: story(core.ResultSuccess, core.ProofService),
			wantSaid: false,
		},
		{
			name:     "an unrecorded previous level never counts as a drop",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofBoot},
			previous: story(core.ResultSuccess, core.ProofUnknown),
			wantSaid: false,
		},
		{
			name:     "an unrecorded current level never counts as a drop",
			state:    core.RunSuccess,
			current:  Story{core.ResultSuccess, core.ProofUnknown},
			previous: story(core.ResultSuccess, core.ProofData),
			wantSaid: false,
		},
		{
			name:     "the workload stopped being evaluable",
			state:    core.RunInconclusive,
			current:  Story{"", core.ProofBoot},
			previous: story(core.ResultSuccess, core.ProofService),
			want:     KindUnevaluable, wantSaid: true,
		},
		{
			name:           "a second inconclusive in a row says nothing",
			state:          core.RunInconclusive,
			current:        Story{"", core.ProofBoot},
			previous:       story(core.ResultSuccess, core.ProofService),
			wasUnevaluable: true,
			wantSaid:       false,
		},
		{
			name:           "we can see the workload again",
			state:          core.RunSuccess,
			current:        Story{core.ResultSuccess, core.ProofService},
			previous:       story(core.ResultSuccess, core.ProofService),
			wasUnevaluable: true,
			want:           KindEvaluable, wantSaid: true,
		},
		{
			name:     "a cancelled run never speaks",
			state:    core.RunCancelled,
			current:  Story{"", core.ProofNone},
			previous: story(core.ResultSuccess, core.ProofService),
			wantSaid: false,
		},
		{
			name:     "a cancelled first run never speaks either",
			state:    core.RunCancelled,
			current:  Story{"", core.ProofNone},
			previous: nil,
			wantSaid: false,
		},
		{
			name:     "a run still in flight never speaks",
			state:    core.RunRestoring,
			current:  Story{"", core.ProofNone},
			previous: story(core.ResultSuccess, core.ProofService),
			wantSaid: false,
		},
		{
			name:     "cleanup failed is terminal and carries its verdict",
			state:    core.RunCleanupFailed,
			current:  Story{core.ResultFailed, core.ProofBoot},
			previous: story(core.ResultSuccess, core.ProofService),
			want:     KindVerdict, wantSaid: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, said := Decide(tc.state, tc.current, tc.previous, tc.wasUnevaluable)
			if said != tc.wantSaid {
				t.Fatalf("Decide said=%v, want %v (transition %+v)", said, tc.wantSaid, got)
			}
			if !said {
				return
			}
			if got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want)
			}
			if got.Headline == "" {
				t.Error("a transition with no headline would render an empty message")
			}
		})
	}
}
