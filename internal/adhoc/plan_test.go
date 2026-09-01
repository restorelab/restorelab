package adhoc

import "testing"

// The API validates the plan before it queues anything, so a request that
// cannot become a drill is a 400 and never a row in the queue. This is what
// makes that possible.
func TestPlanRejectsAnUnusableDrillUpFront(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"no workload", Options{ProviderID: "pve"}},
		{"unparseable check", Options{WorkloadID: "110", ProviderID: "pve", Checks: []string{"nonsense:"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Plan(tc.opts); err == nil {
				t.Fatal("Plan accepted a drill that cannot run")
			}
		})
	}
}

// A drill described by nothing but a workload id is a valid drill: that is
// the whole promise of `recovery test 110`, and POST /recovery-runs makes
// the same promise.
func TestPlanFromNothingButAWorkload(t *testing.T) {
	p, err := Plan(Options{WorkloadID: "110", ProviderID: "proxmox-main"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Name != "adhoc-110" || p.Workload.ID != "110" {
		t.Errorf("plan = %+v, want the ad-hoc plan for workload 110", p)
	}
	if len(p.Checks) != 1 {
		t.Fatalf("checks = %+v, want the default one", p.Checks)
	}
	if p.Checks[0].Retries != DefaultCheckRetries {
		t.Errorf("retries = %d, want %d: a drill always questions a guest that just booted",
			p.Checks[0].Retries, DefaultCheckRetries)
	}
}
