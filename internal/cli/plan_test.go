package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/store"
)

// planDoc is a complete, valid plan document. The comment and the key order
// are load-bearing: `plan show` has to give back exactly what was written.
const planDoc = `# the web tier, restored nightly
name: web-tier
description: nightly drill
workload:
  provider: proxmox-main
  id: "110"
checks:
  - type: tcp
    port: 22
`

func TestPlanApplyCreatesThenUpdates(t *testing.T) {
	a, out, _ := newTestApp(t)

	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web.yaml", planDoc))
	if !strings.Contains(out.String(), "created web-tier") {
		t.Fatalf("first apply should report a creation, got:\n%s", out.String())
	}

	// The same name, a different workload: an upsert, not a second plan.
	out.Reset()
	changed := strings.Replace(planDoc, `id: "110"`, `id: "104"`, 1)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web-v2.yaml", changed))
	if !strings.Contains(out.String(), "updated web-tier to v2") {
		t.Fatalf("second apply should report an update to v2, got:\n%s", out.String())
	}

	plans := listPlans(t, a)
	if len(plans) != 1 {
		t.Fatalf("the catalogue holds %d plans, want 1: apply upserts by name", len(plans))
	}
	if plans[0].WorkloadID != "104" {
		t.Errorf("WorkloadID = %q, want 104: the stored plan must follow the file", plans[0].WorkloadID)
	}
}

// A document that cannot become a plan must not become a row either: the
// error is the whole point, and a half-stored plan is what somebody has to
// explain three months later.
func TestPlanApplyRefusesAnInvalidFileAndWritesNothing(t *testing.T) {
	a, _, _ := newTestApp(t)
	path := writePlanDoc(t, "broken.yaml", "name: broken\nworkload:\n  id: \"\"\n")

	cmd := newPlanCmd(a)
	cmd.SetArgs([]string{"apply", path})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("plan apply accepted a plan with no workload id")
	}

	if plans := listPlans(t, a); len(plans) != 0 {
		t.Fatalf("the catalogue holds %d plans after a refusal, want none", len(plans))
	}
}

// A directory of plans under git is the normal case, so one call takes them
// all and says what it did to each.
func TestPlanApplyAppliesEveryFileItIsGiven(t *testing.T) {
	a, out, _ := newTestApp(t)
	second := strings.Replace(planDoc, "name: web-tier", "name: db-tier", 1)

	runCLI(t, newPlanCmd(a), "apply",
		writePlanDoc(t, "web.yaml", planDoc),
		writePlanDoc(t, "db.yaml", second))

	for _, want := range []string{"created web-tier", "created db-tier"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("apply should report %q, got:\n%s", want, out.String())
		}
	}
	if plans := listPlans(t, a); len(plans) != 2 {
		t.Fatalf("the catalogue holds %d plans, want 2", len(plans))
	}
}

func TestPlanListShowsNameWorkloadAndVersion(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web.yaml", planDoc))

	out.Reset()
	runCLI(t, newPlanCmd(a), "list")

	for _, want := range []string{"NAME", "WORKLOAD", "VERSION", "web-tier", "110", "v1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing should mention %q, got:\n%s", want, out.String())
		}
	}
}

func TestPlanListSaysWhenTheCatalogueIsEmpty(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "list")

	got := strings.ToLower(out.String())
	if !strings.Contains(got, "no plan is stored") {
		t.Fatalf("an empty catalogue must say so plainly, got: %q", got)
	}
	// And say how to fill it, the way the empty run listing does.
	if !strings.Contains(got, "plan apply") {
		t.Errorf("an empty catalogue should point at the command that fills it, got: %q", got)
	}
}

// Verbatim means verbatim: the comment survives, and so does the byte count.
// Storing a plan must never rewrite what an operator wrote.
func TestPlanShowPrintsTheDocumentVerbatim(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web.yaml", planDoc))

	out.Reset()
	runCLI(t, newPlanCmd(a), "show", "web-tier")

	if out.String() != planDoc {
		t.Fatalf("plan show rewrote the document:\n--- got ---\n%s\n--- want ---\n%s", out.String(), planDoc)
	}
}

func TestPlanDeleteRemovesItAndSaysSoWhenItIsAlreadyGone(t *testing.T) {
	a, out, _ := newTestApp(t)
	runCLI(t, newPlanCmd(a), "apply", writePlanDoc(t, "web.yaml", planDoc))

	out.Reset()
	runCLI(t, newPlanCmd(a), "delete", "web-tier")
	if !strings.Contains(out.String(), "web-tier") {
		t.Errorf("delete should name what it removed, got:\n%s", out.String())
	}
	if plans := listPlans(t, a); len(plans) != 0 {
		t.Fatalf("the catalogue still holds %d plans after a delete", len(plans))
	}

	cmd := newPlanCmd(a)
	cmd.SetArgs([]string{"delete", "web-tier"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("deleting a plan twice succeeded; the second one has nothing to remove")
	}
	if !strings.Contains(err.Error(), "web-tier") {
		t.Errorf("the refusal should name the plan it could not find, got: %v", err)
	}
}

// `plan validate` is what a CI runs before it applies anything, on a machine
// with no RestoreLab configuration and no database at all. A store that
// refuses every call proves it never reaches for one.
func TestPlanValidateWorksWithoutADatabase(t *testing.T) {
	out := &strings.Builder{}
	a := &app{out: out, err: &strings.Builder{}, noColor: true}
	a.storeOnce.Do(func() { a.storeValue = store.Noop{} })

	runCLI(t, newPlanCmd(a), "validate", writePlanDoc(t, "web.yaml", planDoc))

	if !strings.Contains(out.String(), "web-tier") {
		t.Fatalf("validate should name the plan it accepted, got:\n%s", out.String())
	}
}

func TestPlanValidateRefusesABrokenFile(t *testing.T) {
	a := &app{out: &strings.Builder{}, err: &strings.Builder{}, noColor: true}
	a.storeOnce.Do(func() { a.storeValue = store.Noop{} })

	cmd := newPlanCmd(a)
	cmd.SetArgs([]string{"validate", writePlanDoc(t, "broken.yaml", "name: broken\nnot_a_field: 1\n")})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("validate accepted a document with an unknown field")
	}
}

// A file and a stored name are two different plans. Picking one silently is
// how the wrong drill runs against a production cluster.
func TestRecoveryRunRefusesAFileAndAStoredPlanTogether(t *testing.T) {
	a, _, _ := newTestApp(t)

	cmd := newRecoveryRunCmd(a)
	cmd.SetArgs([]string{writePlanDoc(t, "web.yaml", planDoc), "--plan", "web-tier"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("recovery run accepted both a file and --plan; it must refuse rather than guess")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("the refusal should say the two are exclusive, got: %v", err)
	}
}

// And with neither, it says what it wanted instead of failing on an empty
// path somewhere further down.
func TestRecoveryRunWithNoPlanAtAllSaysWhatItNeeds(t *testing.T) {
	a, _, _ := newTestApp(t)

	cmd := newRecoveryRunCmd(a)
	cmd.SetArgs(nil)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("recovery run with no plan succeeded")
	}
	if !strings.Contains(err.Error(), "--plan") {
		t.Errorf("the error should mention the stored-plan flag, got: %v", err)
	}
}

// --- helpers -----------------------------------------------------------------

// writePlanDoc drops a plan document in a temporary directory and returns its
// path.
func writePlanDoc(t *testing.T, name, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// listPlans reads the catalogue straight from the store, so an assertion
// about what was written never goes through the command being tested.
func listPlans(t *testing.T, a *app) []store.Plan {
	t.Helper()
	ctx := context.Background()
	plans, err := a.store(ctx).ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		t.Fatalf("listing plans: %v", err)
	}
	return plans
}
