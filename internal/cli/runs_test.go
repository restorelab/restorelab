package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

func TestRenderRunListShowsTheEssentials(t *testing.T) {
	var sb strings.Builder
	renderRunList(&sb, []store.RunSummary{{
		ID:               "0aca8405-4e80-4ac9-8bdd-057a56dc0281",
		PlanName:         "adhoc-110",
		SourceWorkloadID: "110",
		SourceName:       "linux-test",
		State:            core.RunSuccess,
		Result:           core.ResultSuccess,
		StartedAt:        time.Date(2026, 9, 1, 0, 58, 42, 0, time.UTC),
		RTO:              28 * time.Second,
		CleanupDone:      true,
	}})
	out := sb.String()

	for _, want := range []string{"0aca8405", "110", "linux-test", "SUCCESS"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing should mention %q, got:\n%s", want, out)
		}
	}
	// The full uuid is noise in a table; the short form is what gets retyped.
	if strings.Contains(out, "0aca8405-4e80-4ac9-8bdd-057a56dc0281") {
		t.Errorf("the listing should show a short id, not the full uuid:\n%s", out)
	}
}

// A workload that was kept must say so loudly: it is still running on the
// cluster and someone has to remove it.
func TestRenderRunListFlagsAKeptWorkload(t *testing.T) {
	var sb strings.Builder
	renderRunList(&sb, []store.RunSummary{{
		ID: "abcdef1234", SourceWorkloadID: "110",
		Result: core.ResultSuccess, StartedAt: time.Now(), CleanupDone: false,
	}})

	if !strings.Contains(sb.String(), "KEPT") {
		t.Fatalf("a workload left behind must be visible in the listing, got:\n%s", sb.String())
	}
}

// An unfinished run has no verdict; showing its state beats showing a blank.
func TestRenderRunListFallsBackToTheStateWhenThereIsNoVerdict(t *testing.T) {
	var sb strings.Builder
	renderRunList(&sb, []store.RunSummary{{
		ID: "abcdef1234", SourceWorkloadID: "110",
		State: core.RunRestoring, StartedAt: time.Now(),
	}})

	if !strings.Contains(sb.String(), "RESTORING") {
		t.Fatalf("a run with no verdict should show its state, got:\n%s", sb.String())
	}
}

func TestRenderRunListSaysWhenThereIsNothing(t *testing.T) {
	var sb strings.Builder
	renderRunList(&sb, nil)

	out := strings.ToLower(sb.String())
	if !strings.Contains(out, "no drill") {
		t.Fatalf("an empty history must say so plainly, got: %q", sb.String())
	}
	// And it should say how to make one.
	if !strings.Contains(out, "recovery test") {
		t.Errorf("an empty history should point at the command that fills it, got: %q", sb.String())
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("0aca8405-4e80-4ac9"); got != "0aca8405" {
		t.Errorf("shortID = %q, want %q", got, "0aca8405")
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID on a short id = %q, want it unchanged", got)
	}
}

func TestParseSinceAcceptsDurationsAndDates(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	got, err := parseSince("7d", now)
	if err != nil {
		t.Fatalf("parseSince(7d): %v", err)
	}
	if want := now.AddDate(0, 0, -7); !got.Equal(want) {
		t.Errorf("parseSince(7d) = %v, want %v", got, want)
	}

	got, err = parseSince("12h", now)
	if err != nil {
		t.Fatalf("parseSince(12h): %v", err)
	}
	if want := now.Add(-12 * time.Hour); !got.Equal(want) {
		t.Errorf("parseSince(12h) = %v, want %v", got, want)
	}

	got, err = parseSince("2026-08-01", now)
	if err != nil {
		t.Fatalf("parseSince(date): %v", err)
	}
	if got.Year() != 2026 || got.Month() != time.August || got.Day() != 1 {
		t.Errorf("parseSince(2026-08-01) = %v", got)
	}
}

// Silently listing the wrong window is worse than refusing.
func TestParseSinceRefusesWhatItCannotRead(t *testing.T) {
	now := time.Now()
	for _, bad := range []string{"last tuesday", "", "7 days", "-3d", "tomorrow"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) succeeded, want an error", bad)
		}
	}
}
