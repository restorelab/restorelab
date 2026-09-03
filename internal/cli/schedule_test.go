package cli

import (
	"strings"
	"testing"
)

const scheduledPlanDoc = `name: linux-nightly
workload:
  provider: proxmox-main
  id: "110"
schedule: "0 3 * * *"
schedule_timezone: UTC
checks:
  - type: command
    run: systemctl is-active ssh
    expect: active
`

func TestScheduleListShowsAScheduledPlanAndItsNextSlot(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "nightly.yaml", scheduledPlanDoc))

	out.Reset()
	runCLI(t, newScheduleCmd(a), "list")

	got := out.String()
	for _, want := range []string{"linux-nightly", "110", "0 3 * * *", "in "} {
		if !strings.Contains(got, want) {
			t.Fatalf("schedule list does not mention %q, got:\n%s", want, got)
		}
	}
	// A plan that has never been drilled must say so, rather than leave the
	// column blank: "never tested" and "tested badly" are different answers.
	if !strings.Contains(got, "never") {
		t.Fatalf("schedule list does not say the plan has no slot history, got:\n%s", got)
	}
}

func TestScheduleListLeavesOutPlansWithNoSchedule(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web.yaml", planDoc))

	out.Reset()
	runCLI(t, newScheduleCmd(a), "list")

	got := out.String()
	if strings.Contains(got, "web-tier") {
		t.Fatalf("schedule list shows an unscheduled plan, got:\n%s", got)
	}
	// And it says why the listing is empty, rather than printing a bare
	// header somebody has to interpret.
	if !strings.Contains(got, "No stored plan carries a schedule") {
		t.Fatalf("schedule list does not explain an empty listing, got:\n%s", got)
	}
}

func TestScheduleSlotsIsEmptyBeforeTheSchedulerHasRun(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "nightly.yaml", scheduledPlanDoc))

	out.Reset()
	runCLI(t, newScheduleCmd(a), "slots")

	if !strings.Contains(out.String(), "not decided any slot yet") {
		t.Fatalf("schedule slots does not explain an empty listing, got:\n%s", out.String())
	}
}
