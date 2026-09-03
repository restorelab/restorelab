package diag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// fakeProvider is the smallest hypervisor the diagnostic can run against.
// Its destructive methods fail the test if anything ever calls them: a
// diagnostic that could restore or delete a workload would be a bug of a
// different order than a wrong finding.
type fakeProvider struct {
	t *testing.T

	pingErr   error
	nodes     []core.Node
	nodesErr  error
	workload  *core.Workload
	statusErr error
	status    *core.WorkloadStatus
}

func (f *fakeProvider) ID() string   { return "fake" }
func (f *fakeProvider) Kind() string { return "fake" }

func (f *fakeProvider) Ping(context.Context) error { return f.pingErr }

func (f *fakeProvider) ListNodes(context.Context) ([]core.Node, error) {
	return f.nodes, f.nodesErr
}

func (f *fakeProvider) ListWorkloads(context.Context) ([]core.Workload, error) {
	if f.workload == nil {
		return nil, nil
	}
	return []core.Workload{*f.workload}, nil
}

func (f *fakeProvider) GetWorkload(_ context.Context, id string) (*core.Workload, error) {
	if f.workload == nil || f.workload.ID != id {
		return nil, core.ErrNotFound
	}
	return f.workload, nil
}

func (f *fakeProvider) GetStatus(context.Context, string) (*core.WorkloadStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeProvider) AllocateWorkloadID(context.Context) (string, error) {
	f.t.Fatal("the diagnostic allocated a workload id: it must never prepare a restore")
	return "", nil
}

func (f *fakeProvider) Restore(context.Context, core.Backup, core.RestoreOptions) (*core.RestoreJob, error) {
	f.t.Fatal("the diagnostic called Restore")
	return nil, nil
}

func (f *fakeProvider) WaitForJob(context.Context, *core.RestoreJob) (*core.TaskState, error) {
	f.t.Fatal("the diagnostic waited on a job")
	return nil, nil
}

func (f *fakeProvider) Start(context.Context, string) error {
	f.t.Fatal("the diagnostic started a workload")
	return nil
}

func (f *fakeProvider) Stop(context.Context, string) error {
	f.t.Fatal("the diagnostic stopped a workload")
	return nil
}

func (f *fakeProvider) Delete(context.Context, string) error {
	f.t.Fatal("the diagnostic deleted a workload")
	return nil
}

// isolationValidator lets a fake answer the network question.
type isolationValidator struct {
	*fakeProvider
	err error
}

func (v isolationValidator) ValidateIsolation(context.Context, string, core.NetworkConfig) error {
	return v.err
}

func onlineNode(id string) core.Node { return core.Node{ID: id, Name: id, Online: true} }

func findingsIn(r Report, area string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Area == area {
			out = append(out, f)
		}
	}
	return out
}

func TestUnreachableAPIStopsTheDiagnostic(t *testing.T) {
	p := &fakeProvider{t: t, pingErr: errors.New("dial tcp: connection refused")}

	r := Run(context.Background(), Input{Provider: p, ProviderID: "pve", Endpoint: "https://x:8006"})

	if r.OK() {
		t.Fatal("an unreachable API must not report a ready cluster")
	}
	if len(findingsIn(r, "credentials")) != 1 {
		t.Fatalf("want exactly one credentials finding, got %+v", r.Findings)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("the diagnostic kept going after the API was unreachable: %+v", r.Findings)
	}
}

func TestAnIsolatedBridgeIsAPass(t *testing.T) {
	p := &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}

	r := Run(context.Background(), Input{
		Provider:    isolationValidator{fakeProvider: p},
		ProviderID:  "pve",
		NetworkName: "isolated",
		Network:     core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	net := findingsIn(r, "network")
	if len(net) != 1 || net[0].Level != LevelOK {
		t.Fatalf("network findings = %+v, want one ok", net)
	}
}

func TestUnverifiableIsolationWarnsAndDoesNotFail(t *testing.T) {
	p := &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}

	r := Run(context.Background(), Input{
		Provider:    isolationValidator{fakeProvider: p, err: core.ErrIsolationUnverified},
		ProviderID:  "pve",
		NetworkName: "isolated",
		Network:     core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	net := findingsIn(r, "network")
	if len(net) != 1 || net[0].Level != LevelWarn {
		t.Fatalf("network findings = %+v, want one warn", net)
	}
	// "I could not check" is not "it is unsafe": a drill still proceeds on
	// the plan's assertion, so this must not count as a problem.
	if r.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0: unverifiable isolation is a warning", r.Problems())
	}
}

func TestAKnownUnsafeBridgeFails(t *testing.T) {
	p := &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}

	r := Run(context.Background(), Input{
		Provider:    isolationValidator{fakeProvider: p, err: core.ErrNetworkNotIsolated},
		ProviderID:  "pve",
		NetworkName: "isolated",
		Network:     core.NetworkConfig{Bridge: "vmbr0", Isolated: true},
	})

	net := findingsIn(r, "network")
	if len(net) != 1 || net[0].Level != LevelFail {
		t.Fatalf("network findings = %+v, want one fail", net)
	}
}

func TestANonIsolatedProfileFailsWithoutAskingTheCluster(t *testing.T) {
	p := &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}

	r := Run(context.Background(), Input{
		Provider:    p, // no validator: the profile alone decides
		ProviderID:  "pve",
		NetworkName: "prod",
		Network:     core.NetworkConfig{Bridge: "vmbr0", Isolated: false},
	})

	net := findingsIn(r, "network")
	if len(net) != 1 || net[0].Level != LevelFail {
		t.Fatalf("network findings = %+v, want one fail", net)
	}
	if !strings.Contains(net[0].Title, "not marked isolated") {
		t.Errorf("finding = %q, want it to name the profile problem", net[0].Title)
	}
}

func TestAWorkloadWithNoAgentIsAWarningNotAFailure(t *testing.T) {
	p := &fakeProvider{
		t:        t,
		nodes:    []core.Node{onlineNode("pve1")},
		workload: &core.Workload{ID: "110", Name: "linux-test", Node: "pve1"},
		status:   &core.WorkloadStatus{ID: "110", PowerState: core.PowerStateRunning},
	}

	r := Run(context.Background(), Input{
		Provider: p, ProviderID: "pve", WorkloadID: "110",
		Network: core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	var sawAgentWarning bool
	for _, f := range findingsIn(r, "workload") {
		if f.Level == LevelWarn && strings.Contains(f.Title, "guest agent") {
			sawAgentWarning = true
		}
		if f.Level == LevelFail {
			t.Errorf("a missing agent must not be a failure: %+v", f)
		}
	}
	if !sawAgentWarning {
		t.Fatalf("no guest agent warning in %+v", findingsIn(r, "workload"))
	}
}

func TestAWorkloadWithNoBackupFails(t *testing.T) {
	p := &fakeProvider{
		t:        t,
		nodes:    []core.Node{onlineNode("pve1")},
		workload: &core.Workload{ID: "110", Name: "linux-test", Node: "pve1"},
		status:   &core.WorkloadStatus{ID: "110", PowerState: core.PowerStateRunning, AgentReady: true},
	}

	r := Run(context.Background(), Input{
		Provider: p, ProviderID: "pve", WorkloadID: "110",
		Backups: fakeBackups{err: core.ErrNoBackup},
		Network: core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	if r.Problems() == 0 {
		t.Fatal("a workload with no backup must be a problem: there is nothing to recovery-test")
	}
}

// fakeBackups answers the backup lookup.
type fakeBackups struct {
	backup *core.Backup
	err    error
}

func (f fakeBackups) ID() string                 { return "fake" }
func (f fakeBackups) Kind() string               { return "fake" }
func (f fakeBackups) Ping(context.Context) error { return nil }

func (f fakeBackups) ListBackups(context.Context, string) ([]core.Backup, error) {
	if f.backup == nil {
		return nil, f.err
	}
	return []core.Backup{*f.backup}, f.err
}

func (f fakeBackups) GetLatestBackup(context.Context, string) (*core.Backup, error) {
	return f.backup, f.err
}

func TestARecentBackupIsAPass(t *testing.T) {
	backup := &core.Backup{ID: "local:backup/vzdump-qemu-110.vma.zst", CreatedAt: time.Now().Add(-2 * time.Hour)}
	p := &fakeProvider{
		t:        t,
		nodes:    []core.Node{onlineNode("pve1")},
		workload: &core.Workload{ID: "110", Name: "linux-test", Node: "pve1"},
		status:   &core.WorkloadStatus{ID: "110", PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.0.0.5"}},
	}

	r := Run(context.Background(), Input{
		Provider: p, ProviderID: "pve", WorkloadID: "110",
		Backups: fakeBackups{backup: backup},
		Network: core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	if r.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0: %+v", r.Problems(), r.Findings)
	}
	if !r.OK() {
		t.Fatal("OK() = false with no problem finding")
	}
}

// doctor exists to tell somebody what will bite them before it does. The
// isolation itself is working as designed here - which is exactly why nobody
// expects it to be the reason their first drill sat timing out for five
// minutes and then reported a good backup as broken.
func TestAnIsolatedBridgeExplainsWhatItMeansForChecks(t *testing.T) {
	p := &fakeProvider{t: t, nodes: []core.Node{onlineNode("pve1")}}

	r := Run(context.Background(), Input{
		Provider:    isolationValidator{fakeProvider: p},
		ProviderID:  "pve",
		NetworkName: "isolated",
		Network:     core.NetworkConfig{Bridge: "vmbr99", Isolated: true},
	})

	net := findingsIn(r, "network")
	if len(net) != 1 {
		t.Fatalf("network findings = %+v, want one", net)
	}
	// Still not a problem: a network nothing can reach is the product
	// working. Counting it as one would be its own kind of crying wolf.
	if net[0].Level != LevelOK {
		t.Errorf("Level = %s, want ok: an unreachable recovery network is by design", net[0].Level)
	}
	if r.Problems() != 0 {
		t.Errorf("Problems() = %d, want 0", r.Problems())
	}

	for _, want := range []string{"cmd:", "INCONCLUSIVE", "route"} {
		if !strings.Contains(net[0].Detail, want) {
			t.Errorf("detail does not mention %q, so it does not actually help:\n%s", want, net[0].Detail)
		}
	}
}
