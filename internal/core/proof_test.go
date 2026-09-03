package core

import "testing"

// The ladder is ordered by what is established, not by what is expensive to
// arrange. A network probe needs a route into the recovery network and a
// query against the data does not - and the query still proves more.
func TestProofLevelsAreOrderedByWhatTheyEstablish(t *testing.T) {
	ordered := []ProofLevel{ProofNone, ProofBoot, ProofService, ProofData}
	for i := 1; i < len(ordered); i++ {
		if !ordered[i].AtLeast(ordered[i-1]) || ordered[i].Rank() == ordered[i-1].Rank() {
			t.Errorf("%s does not outrank %s", ordered[i], ordered[i-1])
		}
	}
}

// An unrecorded level is not "nothing was proven". It ranks below every real
// level so it never wins a comparison, and Recorded() reports it as what it
// is, so a caller can refuse to draw any conclusion from it at all. This is
// what keeps a run from before the feature from marking a workload down.
func TestUnknownRanksBelowEverythingAndIsNotNone(t *testing.T) {
	if ProofUnknown.Rank() >= ProofNone.Rank() {
		t.Errorf("unknown ranks %d, none ranks %d: unknown must rank lower",
			ProofUnknown.Rank(), ProofNone.Rank())
	}
	if ProofUnknown.Recorded() {
		t.Error("Recorded() = true for the zero value")
	}
	if ProofUnknown == ProofNone {
		t.Error("unknown and none are the same value: they are different claims")
	}
	if got := ProofUnknown.Raise(ProofBoot); got != ProofBoot {
		t.Errorf("Raise(unknown, boot) = %s, want BOOT", got)
	}
}

// A level read back from a newer RestoreLab that this build does not know
// must not be mistaken for a real one - it would rank -1 and cap nothing,
// which is the prudent direction, but the parse has to say so.
func TestParseRefusesAnUnknownLevelButAcceptsTheEmptyOne(t *testing.T) {
	if _, ok := ParseProofLevel("TRANSCENDENT"); ok {
		t.Error("ParseProofLevel accepted a level that does not exist")
	}
	l, ok := ParseProofLevel("")
	if !ok || l != ProofUnknown {
		t.Errorf("ParseProofLevel(\"\") = %q, %v; want unknown, true", l, ok)
	}
	if l, ok := ParseProofLevel("SERVICE"); !ok || l != ProofService {
		t.Errorf("ParseProofLevel(\"SERVICE\") = %q, %v", l, ok)
	}
}

func pc(level ProofLevel, status CheckStatus) ProofCheck {
	return ProofCheck{
		Config: CheckConfig{Proves: level},
		Result: CheckResult{Status: status},
	}
}

func TestProvenBy(t *testing.T) {
	tests := []struct {
		name   string
		checks []ProofCheck
		want   ProofLevel
	}{
		{
			name: "no checks at all establish nothing",
			want: ProofNone,
		},
		{
			// The whole point of the slice: the drill went perfectly and
			// proved that the kernel boots.
			name:   "a passing liveness check proves the boot and no more",
			checks: []ProofCheck{pc(ProofBoot, CheckPass)},
			want:   ProofBoot,
		},
		{
			name:   "the highest passing check wins",
			checks: []ProofCheck{pc(ProofBoot, CheckPass), pc(ProofService, CheckPass)},
			want:   ProofService,
		},
		{
			// This is the counterpart of C5, and the case most worth pinning:
			// a service check that came back bad tells you the service is
			// bad. It does not tell you the service was verified.
			name:   "a failing service check proves nothing, the boot beside it still does",
			checks: []ProofCheck{pc(ProofBoot, CheckPass), pc(ProofService, CheckFail)},
			want:   ProofBoot,
		},
		{
			// A check that could not run is exactly the case RunInconclusive
			// exists for: no route into the isolated network says nothing
			// about the backup, in either direction.
			name:   "a check that could not run proves nothing",
			checks: []ProofCheck{pc(ProofService, CheckError)},
			want:   ProofNone,
		},
		{
			name:   "a check that never ran proves nothing",
			checks: []ProofCheck{pc(ProofService, CheckSkipped)},
			want:   ProofNone,
		},
		{
			name:   "a passing check with no declared level contributes nothing",
			checks: []ProofCheck{pc(ProofUnknown, CheckPass)},
			want:   ProofNone,
		},
		{
			name:   "data outranks the service check beside it",
			checks: []ProofCheck{pc(ProofService, CheckPass), pc(ProofData, CheckPass)},
			want:   ProofData,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProvenBy(tc.checks); got != tc.want {
				t.Errorf("ProvenBy() = %s, want %s", got, tc.want)
			}
		})
	}
}

// Every level has to be able to finish the sentence "this drill established
// ...", because the level is never shown on its own: a bare BOOT beside a
// score means nothing to somebody who has not read the documentation.
func TestEveryLevelDescribesItself(t *testing.T) {
	for _, l := range []ProofLevel{ProofUnknown, ProofNone, ProofBoot, ProofService, ProofData} {
		if l.Describe() == "" {
			t.Errorf("%q describes itself as nothing", l)
		}
	}
}
