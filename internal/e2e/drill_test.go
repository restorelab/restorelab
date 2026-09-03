// Package e2e wires the real Proxmox provider, the real check registry, the
// real recovery engine and the real report renderer together against an
// in-process Proxmox API, and drives a complete recovery drill through them.
//
// It is the test that answers the question the product exists to answer:
// does the whole chain (find backup, restore, harden, boot, reach the guest,
// validate the service, measure the RTO, clean up) actually work?
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/report"
)

const (
	sourceVMID  = "101"
	tempVMID    = "9000"
	node        = "pve1"
	backupVolid = "pbs-main:backup/vm/101/2026-08-31T03:00:00Z"
)

// fakePVE is a small stateful Proxmox VE. It is deliberately not a mock with
// canned responses: the restore has to actually create a VM that the later
// calls can then see, which is what makes the test meaningful.
type fakePVE struct {
	mu sync.Mutex

	vms      map[string]*fakeVM
	requests []recordedRequest

	guestIP string
	// failStart makes the start task fail, to exercise the failure path.
	failStart bool

	// Guest agent exec: agentUp gates whether the agent answers at all, and
	// execResults maps a command line to what running it produces.
	agentUp     bool
	execResults map[string]execResult
	execCalls   []execCall
	nextPID     int
	pids        map[int]execResult
}

type execResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type fakeVM struct {
	id      string
	name    string
	node    string
	running bool
	config  map[string]string
	tags    string
}

// execCall is one guest-agent command the fake received.
type execCall struct {
	Argv  []string
	Input string
}

type recordedRequest struct {
	Method string
	Path   string
	Form   url.Values
	Query  url.Values
}

func newFakePVE(guestIP string) *fakePVE {
	return &fakePVE{
		guestIP:     guestIP,
		agentUp:     true,
		execResults: map[string]execResult{},
		pids:        map[int]execResult{},
		vms: map[string]*fakeVM{
			sourceVMID: {
				id:     sourceVMID,
				name:   "postgres-prod",
				node:   node,
				config: map[string]string{"net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0", "net1": "virtio,bridge=vmbr1"},
			},
		},
	}
}

func (f *fakePVE) record(r *http.Request, form url.Values) {
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Form:   form,
		Query:  r.URL.Query(),
	})
}

func (f *fakePVE) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var form url.Values
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		_ = r.ParseForm()
		form = r.PostForm
	}
	f.record(r, form)

	if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "PVEAPIToken=") {
		http.Error(w, `{"errors":"missing token"}`, http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	data, status := f.route(r, path, form)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakePVE) route(r *http.Request, path string, form url.Values) (any, int) {
	switch {
	case path == "/version":
		return map[string]any{"version": "8.2.2", "release": "8.2"}, http.StatusOK

	case path == "/nodes":
		return []map[string]any{
			{"node": node, "status": "online", "cpu": 0.1, "maxcpu": 16, "mem": 8 << 30, "maxmem": 64 << 30, "disk": 100 << 30, "maxdisk": 1000 << 30},
		}, http.StatusOK

	case path == "/nodes/"+node+"/status":
		return map[string]any{
			"cpu":     0.1,
			"cpuinfo": map[string]any{"cpus": 16},
			"memory":  map[string]any{"total": 64 << 30, "used": 8 << 30, "free": 56 << 30},
			"rootfs":  map[string]any{"total": 1000 << 30, "used": 100 << 30, "free": 900 << 30},
		}, http.StatusOK

	case path == "/nodes/"+node+"/network":
		// vmbr99 has no ports and no gateway: a switch that goes nowhere.
		return []map[string]any{
			{"iface": "vmbr0", "type": "bridge", "bridge_ports": "eno1", "gateway": "10.0.0.1"},
			{"iface": "vmbr99", "type": "bridge", "bridge_ports": ""},
		}, http.StatusOK

	case path == "/cluster/resources":
		return f.resources(), http.StatusOK

	case path == "/cluster/nextid":
		return tempVMID, http.StatusOK

	case path == "/nodes/"+node+"/storage":
		return []map[string]any{{"storage": "pbs-main", "type": "pbs", "content": "backup"}}, http.StatusOK

	case path == "/nodes/"+node+"/storage/pbs-main/content":
		if r.URL.Query().Get("vmid") != sourceVMID {
			return []map[string]any{}, http.StatusOK
		}
		return []map[string]any{{
			"volid":        backupVolid,
			"vmid":         101,
			"ctime":        time.Now().Add(-2 * time.Hour).Unix(),
			"size":         4 << 30,
			"format":       "pbs-vm",
			"protected":    0,
			"verification": map[string]any{"state": "ok"},
		}}, http.StatusOK

	case path == "/nodes/"+node+"/qemu" && r.Method == http.MethodPost:
		return f.restore(form)

	case strings.HasPrefix(path, "/nodes/"+node+"/tasks/"):
		return map[string]any{"status": "stopped", "exitstatus": f.taskExit(path)}, http.StatusOK
	}

	// /nodes/{node}/qemu/{vmid}/...
	const prefix = "/nodes/" + node + "/qemu/"
	if strings.HasPrefix(path, prefix) {
		rest := strings.TrimPrefix(path, prefix)
		id, action, _ := strings.Cut(rest, "/")
		vm, ok := f.vms[id]
		if !ok {
			return nil, http.StatusNotFound
		}
		return f.vmRoute(r, vm, action, form)
	}

	return nil, http.StatusNotFound
}

func (f *fakePVE) vmRoute(r *http.Request, vm *fakeVM, action string, form url.Values) (any, int) {
	switch {
	case action == "config" && r.Method == http.MethodGet:
		cfg := map[string]any{"name": vm.name, "tags": vm.tags}
		for k, v := range vm.config {
			cfg[k] = v
		}
		return cfg, http.StatusOK

	case action == "config" && r.Method == http.MethodPost:
		for _, k := range strings.Split(form.Get("delete"), ",") {
			if k != "" {
				delete(vm.config, k)
			}
		}
		for k, vals := range form {
			if k == "delete" || len(vals) == 0 {
				continue
			}
			switch k {
			case "tags":
				vm.tags = vals[0]
			case "name":
				vm.name = vals[0]
			default:
				vm.config[k] = vals[0]
			}
		}
		return nil, http.StatusOK

	case action == "status/current":
		status := "stopped"
		if vm.running {
			status = "running"
		}
		return map[string]any{"status": status, "uptime": 42, "cpu": 0.05, "mem": 1 << 30}, http.StatusOK

	case action == "status/start":
		if f.failStart {
			return "UPID:pve1:start-fail:" + vm.id + ":root@pam:", http.StatusOK
		}
		vm.running = true
		return "UPID:pve1:start:" + vm.id + ":root@pam:", http.StatusOK

	case action == "status/stop":
		vm.running = false
		return "UPID:pve1:stop:" + vm.id + ":root@pam:", http.StatusOK

	case action == "agent/exec" && r.Method == http.MethodPost:
		if !vm.running || !f.agentUp {
			// PVE answers a missing or silent agent with an untyped 500.
			return "QEMU guest agent is not running", http.StatusInternalServerError
		}
		argv := form["command"]
		if len(argv) == 0 {
			return nil, http.StatusBadRequest
		}
		f.execCalls = append(f.execCalls, execCall{Argv: argv, Input: form.Get("input-data")})

		f.nextPID++
		res, ok := f.execResults[strings.Join(argv, " ")]
		if !ok {
			res = execResult{ExitCode: 127, Stderr: "sh: command not found"}
		}
		f.pids[f.nextPID] = res
		return map[string]any{"pid": f.nextPID}, http.StatusOK

	case action == "agent/exec-status":
		pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
		res, ok := f.pids[pid]
		if !ok {
			return nil, http.StatusNotFound
		}
		return map[string]any{
			"exited":   1,
			"exitcode": res.ExitCode,
			"out-data": res.Stdout,
			"err-data": res.Stderr,
		}, http.StatusOK

	case action == "agent/network-get-interfaces":
		if !vm.running || !f.agentUp {
			return nil, http.StatusInternalServerError
		}
		return map[string]any{"result": []map[string]any{
			{"name": "lo", "ip-addresses": []map[string]any{{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"}}},
			{"name": "eth0", "ip-addresses": []map[string]any{
				{"ip-address": f.guestIP, "ip-address-type": "ipv4"},
				{"ip-address": "fe80::1", "ip-address-type": "ipv6"},
			}},
		}}, http.StatusOK

	case action == "" && r.Method == http.MethodDelete:
		delete(f.vms, vm.id)
		return "UPID:pve1:destroy:" + vm.id + ":root@pam:", http.StatusOK
	}

	return nil, http.StatusNotFound
}

func (f *fakePVE) restore(form url.Values) (any, int) {
	id := form.Get("vmid")
	if id == "" || form.Get("archive") == "" {
		return nil, http.StatusBadRequest
	}
	if _, exists := f.vms[id]; exists {
		// Without force, PVE refuses to overwrite. RestoreLab must never send
		// force, so this branch is a genuine failure if it is ever reached.
		return nil, http.StatusBadRequest
	}
	// The restored VM inherits the production network configuration from the
	// backup: this is precisely what FinalizeRestore has to strip.
	f.vms[id] = &fakeVM{
		id:   id,
		name: "postgres-prod",
		node: node,
		config: map[string]string{
			"net0":       "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
			"net1":       "virtio=AA:BB:CC:DD:EE:00,bridge=vmbr1",
			"onboot":     "1",
			"protection": "1",
		},
	}
	return "UPID:pve1:qmrestore:" + id + ":root@pam:", http.StatusOK
}

func (f *fakePVE) taskExit(path string) string {
	if strings.Contains(path, "start-fail") {
		return "start failed: got timeout"
	}
	return "OK"
}

func (f *fakePVE) resources() []map[string]any {
	out := make([]map[string]any, 0, len(f.vms))
	for _, vm := range f.vms {
		status := "stopped"
		if vm.running {
			status = "running"
		}
		out = append(out, map[string]any{
			"type": "qemu", "vmid": vm.id, "name": vm.name, "node": vm.node,
			"status": status, "maxcpu": 2, "maxmem": 4 << 30, "maxdisk": 32 << 30,
			"template": 0, "tags": vm.tags,
		})
	}
	return out
}

// --- harness -----------------------------------------------------------------

func newDrill(t *testing.T, guestIP string) (*fakePVE, *proxmox.Provider) {
	t.Helper()

	pve := newFakePVE(guestIP)
	srv := httptest.NewServer(pve)
	t.Cleanup(srv.Close)

	p, err := proxmox.New(proxmox.Config{
		ID:          "proxmox-test",
		Endpoint:    srv.URL,
		TokenID:     "restorelab@pve!drills",
		TokenSecret: "s3cr3t",
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("proxmox.New: %v", err)
	}
	return pve, p
}

func newEngine(t *testing.T, p *proxmox.Provider, events *[]recovery.Event) *recovery.Engine {
	t.Helper()

	e, err := recovery.New(recovery.Deps{
		Hypervisor: p,
		Backups:    p,
		Checks:     checks.Default(),
		// Near-instant sleeps: the guest in this test is ready immediately, and
		// a test should never spend three seconds proving that.
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
				return nil
			}
		},
		Emit: func(ev recovery.Event) {
			if events != nil {
				*events = append(*events, ev)
			}
		},
	})
	if err != nil {
		t.Fatalf("recovery.New: %v", err)
	}
	return e
}

// listenTCP starts a real listener so the tcp check has something true to
// discover, and returns its host and port.
func listenTCP(t *testing.T) (host string, port int) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	addr := l.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// guestAddr is what the fake guest agent reports. It is deliberately NOT a
// loopback address: the provider filters those out, exactly as it should.
const guestAddr = "10.99.0.14"

// closedTCPPort returns a port that nothing is listening on, by taking one
// from the OS and immediately giving it back.
func closedTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func drillPlan(port int, rto time.Duration) *plan.Plan {
	p := &plan.Plan{
		Name:     "postgres-prod",
		Workload: plan.WorkloadRef{Provider: "proxmox-test", ID: sourceVMID},
		Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest, MaxAge: plan.Duration(26 * time.Hour)},
		Restore:  plan.RestoreSpec{Node: node, CPULimit: 2, MemoryLimitMB: 4096},
		Startup:  plan.StartupSpec{Timeout: plan.Duration(30 * time.Second)},
		Checks: []plan.CheckSpec{
			// The check connects to the real local listener rather than to the
			// address the guest agent reports: this process cannot bind
			// 10.99.0.14. Discovery of that address is asserted separately.
			{Type: "tcp", Name: "service port", Params: map[string]any{"host": "127.0.0.1", "port": port}},
		},
		RTOTarget: plan.Duration(rto),
	}
	p.ApplyDefaults()
	return p
}

func isolatedNetwork() core.NetworkConfig {
	return core.NetworkConfig{Bridge: "vmbr99", Isolated: true}
}

// --- the tests ---------------------------------------------------------------

func TestFullDrillSucceedsAndCleansUp(t *testing.T) {
	_, port := listenTCP(t)
	pve, provider := newDrill(t, guestAddr)

	var events []recovery.Event
	engine := newEngine(t, provider, &events)

	run, err := engine.Run(context.Background(), drillPlan(port, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if run.Result != core.ResultSuccess {
		t.Errorf("Result = %s, want SUCCESS (err=%s, checks=%+v)", run.Result, run.Err, run.Checks)
	}
	if run.State != core.RunSuccess {
		t.Errorf("State = %s, want SUCCESS", run.State)
	}
	if run.TempWorkloadID != tempVMID {
		t.Errorf("TempWorkloadID = %q, want %q", run.TempWorkloadID, tempVMID)
	}
	if run.Backup == nil || run.Backup.ID != backupVolid {
		t.Errorf("Backup = %+v, want the volid discovered from the storage", run.Backup)
	}
	if run.RTO <= 0 {
		t.Error("RTO must be measured")
	}
	if run.RTOExceeded() {
		t.Errorf("RTO %v should not exceed the 10m target", run.RTO)
	}
	if !run.CleanupDone {
		t.Error("CleanupDone = false, want the temporary workload destroyed")
	}
	if len(run.Checks) != 1 || !run.Checks[0].OK() {
		t.Errorf("checks = %+v, want one passing check", run.Checks)
	}
	// A drill that answered on the service port proved the service, and the
	// report is entitled to say so. Not DATA: nothing here looked at a row.
	if run.ProofLevel != core.ProofService {
		t.Errorf("ProofLevel = %s, want SERVICE", run.ProofLevel)
	}

	// The temporary workload must be gone from the cluster.
	if _, exists := pve.vms[tempVMID]; exists {
		t.Error("the temporary workload is still on the cluster after a successful run")
	}
	// The production workload must be untouched.
	if _, exists := pve.vms[sourceVMID]; !exists {
		t.Fatal("the SOURCE workload was destroyed - this is the worst possible bug")
	}

	assertNoDestructiveCallOnSource(t, pve)
	assertHardened(t, pve)
	assertRestoreParams(t, pve)

	// The event stream must be usable to render a live timeline, and it must
	// show the address discovered through the guest agent.
	if len(events) < 6 {
		t.Errorf("got %d events, want one per step at least", len(events))
	}
	var sawGuestAddr bool
	for _, ev := range events {
		if strings.Contains(ev.Message, guestAddr) {
			sawGuestAddr = true
		}
	}
	if !sawGuestAddr {
		t.Errorf("the guest address discovered via the agent (%s) never appeared in the event stream", guestAddr)
	}
}

func TestDrillFailsWhenServiceIsDownButStillCleansUp(t *testing.T) {
	_, port := listenTCP(t)
	pve, provider := newDrill(t, guestAddr)

	// A port nothing listens on: the guest booted, the service did not come
	// back. That is exactly the failure this product exists to catch.
	_ = port
	closedPort := closedTCPPort(t)
	engine := newEngine(t, provider, nil)

	run, err := engine.Run(context.Background(), drillPlan(closedPort, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a failure when a critical check fails")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("Result = %s, want FAILED", run.Result)
	}
	if len(run.FailedChecks()) != 1 {
		t.Errorf("FailedChecks() = %+v, want the tcp check to have failed", run.FailedChecks())
	}
	if !run.CleanupDone {
		t.Error("cleanup must run after a failed check, not only after a success")
	}
	// The service is down and the drill says so - and the guest agent still
	// answered, so the boot is established. "This backup boots but its
	// service did not come back" is more useful than either half alone, and
	// the drill must not throw away the half it did establish.
	if run.ProofLevel != core.ProofBoot {
		t.Errorf("ProofLevel = %s, want BOOT: the check failed, the boot did not", run.ProofLevel)
	}
	if _, exists := pve.vms[tempVMID]; exists {
		t.Error("the temporary workload survived a failed run")
	}
	assertNoDestructiveCallOnSource(t, pve)
}

func TestDrillRefusesNonIsolatedNetworkBeforeTouchingAnything(t *testing.T) {
	_, port := listenTCP(t)
	pve, provider := newDrill(t, guestAddr)
	engine := newEngine(t, provider, nil)

	_, err := engine.Run(context.Background(), drillPlan(port, 0), recovery.RunOptions{
		Network: core.NetworkConfig{Bridge: "vmbr0", Isolated: false},
		Node:    node,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a refusal on a non-isolated network")
	}

	for _, r := range pve.recorded() {
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			t.Fatalf("nothing may be created or destroyed when isolation is refused, got %s %s", r.Method, r.Path)
		}
	}
}

func TestDrillFailsOnStaleBackup(t *testing.T) {
	_, port := listenTCP(t)
	pve, provider := newDrill(t, guestAddr)
	engine := newEngine(t, provider, nil)

	p := drillPlan(port, 0)
	p.Backup.MaxAge = plan.Duration(time.Minute) // the fixture backup is 2h old

	run, err := engine.Run(context.Background(), p, recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a failure on a stale backup")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("Result = %s, want FAILED", run.Result)
	}
	for _, r := range pve.recorded() {
		if r.Method == http.MethodPost {
			t.Fatalf("a stale backup must be caught before anything is created, got %s %s", r.Method, r.Path)
		}
	}
}

func TestReportsRenderFromARealRun(t *testing.T) {
	_, port := listenTCP(t)
	_, provider := newDrill(t, guestAddr)
	engine := newEngine(t, provider, nil)

	run, err := engine.Run(context.Background(), drillPlan(port, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var text bytes.Buffer
	if err := report.Text(&text, run, report.Options{ASCII: true}); err != nil {
		t.Fatalf("report.Text: %v", err)
	}
	for _, want := range []string{"postgres-prod", "SUCCESS", "service port"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report is missing %q:\n%s", want, text.String())
		}
	}

	var jsonBuf bytes.Buffer
	if err := report.JSON(&jsonBuf, run); err != nil {
		t.Fatalf("report.JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &doc); err != nil {
		t.Fatalf("the JSON report must be valid JSON: %v", err)
	}
	if doc["schema"] != report.SchemaVersion {
		t.Errorf("schema = %v, want %q", doc["schema"], report.SchemaVersion)
	}

	var html bytes.Buffer
	if err := report.HTML(&html, run); err != nil {
		t.Fatalf("report.HTML: %v", err)
	}
	if !strings.Contains(html.String(), "postgres-prod") {
		t.Error("the HTML report does not name the workload")
	}
}

// --- assertions --------------------------------------------------------------

// assertNoDestructiveCallOnSource is the single most important assertion in
// this repository: whatever else happens, the production workload is only ever
// read.
func assertNoDestructiveCallOnSource(t *testing.T, pve *fakePVE) {
	t.Helper()

	sourcePath := "/api2/json/nodes/" + node + "/qemu/" + sourceVMID
	for _, r := range pve.recorded() {
		if !strings.HasPrefix(r.Path, sourcePath) {
			continue
		}
		if r.Method != http.MethodGet {
			t.Fatalf("a %s request was made against the source workload: %s", r.Method, r.Path)
		}
	}
}

// assertHardened proves the restored clone was stripped of its production
// network configuration before it was ever powered on.
func assertHardened(t *testing.T, pve *fakePVE) {
	t.Helper()

	var (
		configPost *recordedRequest
		startIndex = -1
		cfgIndex   = -1
	)
	for i, r := range pve.recorded() {
		switch {
		case r.Method == http.MethodPost && r.Path == "/api2/json/nodes/"+node+"/qemu/"+tempVMID+"/config":
			rr := r
			configPost = &rr
			cfgIndex = i
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/"+tempVMID+"/status/start") && startIndex == -1:
			startIndex = i
		}
	}

	if configPost == nil {
		t.Fatal("the restored workload was never hardened")
	}
	if startIndex != -1 && cfgIndex > startIndex {
		t.Fatal("the workload was started BEFORE its production network was removed")
	}

	net0 := configPost.Form.Get("net0")
	if !strings.Contains(net0, "bridge=vmbr99") {
		t.Errorf("net0 = %q, want the isolated bridge", net0)
	}
	if strings.Contains(net0, "AA:BB:CC:DD:EE:FF") {
		t.Errorf("net0 = %q, the production MAC must not be reused", net0)
	}
	if got := configPost.Form.Get("delete"); !strings.Contains(got, "net1") {
		t.Errorf("delete = %q, want the extra production interfaces removed", got)
	}
	if got := configPost.Form.Get("onboot"); got != "0" {
		t.Errorf("onboot = %q, want 0: a clone must never come back after a node reboot", got)
	}
	if got := configPost.Form.Get("protection"); got != "0" {
		t.Errorf("protection = %q, want 0 so cleanup can always destroy it", got)
	}
	desc := configPost.Form.Get("description")
	for _, want := range []string{core.MetadataManaged + "=true", core.MetadataRecoveryRunID, core.MetadataSourceID} {
		if !strings.Contains(desc, want) {
			t.Errorf("description = %q, want it to carry %q", desc, want)
		}
	}
}

func assertRestoreParams(t *testing.T, pve *fakePVE) {
	t.Helper()

	for _, r := range pve.recorded() {
		if r.Method != http.MethodPost || r.Path != "/api2/json/nodes/"+node+"/qemu" {
			continue
		}
		if got := r.Form.Get("vmid"); got != tempVMID {
			t.Errorf("restore vmid = %q, want the temporary id %q", got, tempVMID)
		}
		if got := r.Form.Get("archive"); got != backupVolid {
			t.Errorf("restore archive = %q, want %q", got, backupVolid)
		}
		if r.Form.Has("force") {
			t.Error("restore must never send force: it would overwrite an existing workload")
		}
		if got := r.Form.Get("start"); got != "0" {
			t.Errorf("restore start = %q, want 0: the workload is hardened before it boots", got)
		}
		return
	}
	t.Fatal("no restore request was made")
}

// inGuestPlan validates the restored service with a command run inside the
// guest, which is what lets a drill work with no network path at all into the
// isolated recovery network.
func inGuestPlan(t *testing.T) *plan.Plan {
	t.Helper()

	p := &plan.Plan{
		Name:     "postgres-prod",
		Workload: plan.WorkloadRef{Provider: "proxmox-test", ID: sourceVMID},
		Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest, MaxAge: plan.Duration(26 * time.Hour)},
		Restore:  plan.RestoreSpec{Node: node},
		Startup:  plan.StartupSpec{Timeout: plan.Duration(30 * time.Second)},
		Checks: []plan.CheckSpec{{
			Type:   "command",
			Name:   "PostgreSQL",
			Params: map[string]any{"run": "systemctl is-active postgresql", "expect": "active"},
		}},
	}
	p.ApplyDefaults()

	if p.Startup.WaitsForIP() {
		t.Fatal("a plan with only in-guest checks must not wait for an address")
	}
	if !p.Startup.WaitsForAgent() {
		t.Fatal("a plan with in-guest checks must wait for the guest agent")
	}
	return p
}

func TestFullDrillWithInGuestCheckNeedsNoNetworkPath(t *testing.T) {
	// No listener, and an address the test process could never reach: the
	// whole point is that nothing here talks to the guest over the network.
	pve, provider := newDrill(t, "10.99.0.14")
	pve.execResults["/bin/sh -c systemctl is-active postgresql"] = execResult{Stdout: "active\n"}

	engine := newEngine(t, provider, nil)

	run, err := engine.Run(context.Background(), inGuestPlan(t), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Result != core.ResultSuccess {
		t.Fatalf("Result = %s, want SUCCESS (err=%s, checks=%+v)", run.Result, run.Err, run.Checks)
	}
	if len(run.Checks) != 1 || !run.Checks[0].OK() {
		t.Errorf("checks = %+v, want one passing check", run.Checks)
	}
	// A drill that answered on the service port proved the service, and the
	// report is entitled to say so. Not DATA: nothing here looked at a row.
	if run.ProofLevel != core.ProofService {
		t.Errorf("ProofLevel = %s, want SERVICE", run.ProofLevel)
	}
	if !run.CleanupDone {
		t.Error("cleanup must still run")
	}

	if len(pve.execCalls) != 1 {
		t.Fatalf("execCalls = %+v, want exactly one guest command", pve.execCalls)
	}
	want := []string{"/bin/sh", "-c", "systemctl is-active postgresql"}
	if strings.Join(pve.execCalls[0].Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", pve.execCalls[0].Argv, want)
	}
	assertNoDestructiveCallOnSource(t, pve)
}

// A service that did not come back must fail the drill, not error it: the
// command ran fine, the answer was just wrong.
func TestInGuestCheckFailsWhenServiceIsDown(t *testing.T) {
	pve, provider := newDrill(t, "10.99.0.14")
	pve.execResults["/bin/sh -c systemctl is-active postgresql"] = execResult{ExitCode: 3, Stdout: "inactive\n"}

	engine := newEngine(t, provider, nil)

	run, err := engine.Run(context.Background(), inGuestPlan(t), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a failure")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("Result = %s, want FAILED", run.Result)
	}
	failed := run.FailedChecks()
	if len(failed) != 1 {
		t.Fatalf("FailedChecks() = %+v, want one", failed)
	}
	if failed[0].Status != core.CheckFail {
		t.Errorf("Status = %s, want fail: the command ran, its answer was wrong", failed[0].Status)
	}
	if !strings.Contains(failed[0].Message, "inactive") {
		t.Errorf("message = %q, want the evidence in it", failed[0].Message)
	}
	if !run.CleanupDone {
		t.Error("cleanup must run after a failed check")
	}
}

// No guest agent is "I could not ask", not "your service is broken": a drill
// against a guest whose agent never answers must fail on the wait, with a
// message that says so, rather than accusing a service nobody ever reached.
func TestDrillFailsClearlyWhenTheGuestAgentNeverAnswers(t *testing.T) {
	pve, provider := newDrill(t, "10.99.0.14")
	pve.agentUp = false

	engine := newEngine(t, provider, nil)

	p := inGuestPlan(t)
	p.Startup.Timeout = plan.Duration(2 * time.Second)

	run, err := engine.Run(context.Background(), p, recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a failure when the agent never answers")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("Result = %s, want FAILED", run.Result)
	}
	if !strings.Contains(run.Err, "agent=no") {
		t.Errorf("run error = %q, want it to say the agent never answered", run.Err)
	}
	if len(pve.execCalls) != 0 {
		t.Errorf("no command may be attempted before the agent is up, got %+v", pve.execCalls)
	}
	if !run.CleanupDone {
		t.Error("cleanup must still run")
	}
	if _, exists := pve.vms[tempVMID]; exists {
		t.Error("the temporary workload survived")
	}
}
