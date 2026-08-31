package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// netIfaceRE matches a QEMU network interface config key: net0, net1, ...
var netIfaceRE = regexp.MustCompile(`^net[0-9]+$`)

// ListNodes returns every node in the cluster.
func (p *Provider) ListNodes(ctx context.Context) ([]core.Node, error) {
	raw, err := p.get(ctx, "/nodes", nil)
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode /nodes: %w", err)
	}

	nodes := make([]core.Node, 0, len(entries))
	for _, e := range entries {
		nodes = append(nodes, core.Node{
			ID:               asString(e["node"]),
			Name:             asString(e["node"]),
			Online:           asString(e["status"]) == "online",
			CPUCores:         asInt(e["maxcpu"]),
			CPUUsage:         asFloat(e["cpu"]),
			MemoryTotalBytes: asInt64(e["maxmem"]),
			MemoryUsedBytes:  asInt64(e["mem"]),
			DiskTotalBytes:   asInt64(e["maxdisk"]),
			DiskUsedBytes:    asInt64(e["disk"]),
		})
	}
	return nodes, nil
}

// clusterResources fetches /cluster/resources filtered to the given PVE
// resource type ("vm" for both qemu guests and lxc containers).
func (p *Provider) clusterResources(ctx context.Context, resType string) ([]map[string]any, error) {
	raw, err := p.get(ctx, "/cluster/resources", url.Values{"type": {resType}})
	if err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("proxmox: decode /cluster/resources: %w", err)
	}
	return entries, nil
}

// ListWorkloads returns every qemu VM and lxc container in the cluster.
func (p *Provider) ListWorkloads(ctx context.Context) ([]core.Workload, error) {
	entries, err := p.clusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	workloads := make([]core.Workload, 0, len(entries))
	for _, e := range entries {
		w, ok := mapWorkloadEntry(e)
		if ok {
			workloads = append(workloads, w)
		}
	}
	return workloads, nil
}

func mapWorkloadEntry(e map[string]any) (core.Workload, bool) {
	kind := core.WorkloadKindUnknown
	switch asString(e["type"]) {
	case "qemu":
		kind = core.WorkloadKindVM
	case "lxc":
		kind = core.WorkloadKindContainer
	default:
		return core.Workload{}, false
	}

	tags := splitTags(asString(e["tags"]))
	return core.Workload{
		ID:          idString(e["vmid"]),
		Name:        asString(e["name"]),
		Kind:        kind,
		Node:        asString(e["node"]),
		Tags:        tags,
		CPUCores:    asInt(e["maxcpu"]),
		MemoryBytes: asInt64(e["maxmem"]),
		DiskBytes:   asInt64(e["maxdisk"]),
		PowerState:  mapPowerState(asString(e["status"])),
		Template:    asBool(e["template"]),
		Managed:     containsTag(tags, "restorelab"),
	}, true
}

func mapPowerState(s string) core.PowerState {
	switch s {
	case "running":
		return core.PowerStateRunning
	case "stopped":
		return core.PowerStateStopped
	case "paused", "suspended":
		return core.PowerStatePaused
	default:
		return core.PowerStateUnknown
	}
}

// splitTags splits PVE's semicolon-separated tag string, dropping empties.
func splitTags(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	out := make([]string, 0, len(parts))
	for _, t := range parts {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// resolve finds which node hosts workload id and whether it is a qemu VM or
// an lxc container, by scanning /cluster/resources. It does no caching: PVE
// state (in particular the hosting node, across a migration) can change
// between calls, so every call goes to the API.
func (p *Provider) resolve(ctx context.Context, id string) (node string, kind string, err error) {
	entries, err := p.clusterResources(ctx, "vm")
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if idString(e["vmid"]) == id {
			return asString(e["node"]), asString(e["type"]), nil
		}
	}
	return "", "", fmt.Errorf("proxmox: workload %q: %w", id, core.ErrNotFound)
}

// GetWorkload returns a single workload by ID.
func (p *Provider) GetWorkload(ctx context.Context, id string) (*core.Workload, error) {
	entries, err := p.clusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if idString(e["vmid"]) != id {
			continue
		}
		w, ok := mapWorkloadEntry(e)
		if !ok {
			break
		}
		return &w, nil
	}
	return nil, fmt.Errorf("proxmox: workload %q: %w", id, core.ErrNotFound)
}

// GetStatus returns the live status of a workload, including a best-effort
// attempt at guest-agent-reported IP addresses for qemu VMs. A guest agent
// that is absent, disabled or simply not answering must never fail the
// whole call: AgentReady is left false and IPs empty in that case.
func (p *Provider) GetStatus(ctx context.Context, id string) (*core.WorkloadStatus, error) {
	node, kind, err := p.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/status/current", node, kind, id), nil)
	if err != nil {
		return nil, err
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("proxmox: decode status/current for %s: %w", id, err)
	}

	status := &core.WorkloadStatus{
		ID:          id,
		PowerState:  mapPowerState(asString(s["status"])),
		Uptime:      time.Duration(asInt64(s["uptime"])) * time.Second,
		CPUUsage:    asFloat(s["cpu"]),
		MemoryBytes: asInt64(s["mem"]),
	}

	if kind == "qemu" {
		if ips, ok := p.agentIPs(ctx, node, id); ok {
			status.AgentReady = true
			status.IPs = ips
		}
	}
	return status, nil
}

// agentIPs best-effort queries the QEMU guest agent for its interface list.
// ok is false whenever the agent could not be reached or answered garbage;
// callers must treat that as "unknown", never as an error.
func (p *Provider) agentIPs(ctx context.Context, node, id string) (ips []string, ok bool) {
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/agent/network-get-interfaces", node, id), nil)
	if err != nil {
		return nil, false
	}
	var res agentInterfaces
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, false
	}

	for _, iface := range res.Result {
		for _, addr := range iface.IPAddresses {
			if isUnusableAgentIP(addr.IPAddress) {
				continue
			}
			ips = append(ips, addr.IPAddress)
		}
	}
	return ips, true
}

// isUnusableAgentIP filters loopback (127.0.0.0/8, ::1) and link-local
// (169.254.0.0/16, fe80::/10) addresses out of guest-agent reported IPs;
// neither is ever useful as a reachability target for checks.
func isUnusableAgentIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}
	return parsed.IsLoopback() || parsed.IsLinkLocalUnicast()
}

// AllocateWorkloadID reserves a free workload ID inside
// [cfg.TempIDMin, cfg.TempIDMax]. PVE's /cluster/nextid returns the
// cluster-wide next free ID, which may fall outside that range (it is
// shared with every other VM/CT on the cluster); when it does, this probes
// upward from TempIDMin using /cluster/nextid?vmid=N, which PVE errors on
// when N is already in use.
func (p *Provider) AllocateWorkloadID(ctx context.Context) (string, error) {
	raw, err := p.get(ctx, "/cluster/nextid", nil)
	if err != nil {
		return "", err
	}
	var idStr string
	if err := json.Unmarshal(raw, &idStr); err != nil {
		return "", fmt.Errorf("proxmox: decode /cluster/nextid: %w", err)
	}
	if id, convErr := strconv.Atoi(idStr); convErr == nil && id >= p.cfg.TempIDMin && id <= p.cfg.TempIDMax {
		return idStr, nil
	}

	for n := p.cfg.TempIDMin; n <= p.cfg.TempIDMax; n++ {
		_, err := p.get(ctx, "/cluster/nextid", url.Values{"vmid": {strconv.Itoa(n)}})
		if err == nil {
			return strconv.Itoa(n), nil
		}
		if core.IsRetryable(err) || errors.Is(err, core.ErrUnauthorized) {
			// A real transport/auth problem, not "this ID is taken": surface it.
			return "", err
		}
	}
	return "", fmt.Errorf("proxmox: no free workload id in range %d-%d", p.cfg.TempIDMin, p.cfg.TempIDMax)
}

// Restore creates a new qemu VM at opts.TargetWorkloadID from backup by
// invoking PVE's restore-via-archive form of the VM create call. It never
// passes force=1, so PVE refuses outright if the target ID is somehow
// already in use rather than overwriting it.
//
// Restore only submits the asynchronous PVE task and returns a handle to it;
// call WaitForJob to block until it settles, and once that succeeds call
// FinalizeRestore to strip the restored workload's production network
// configuration and stamp RestoreLab's ownership metadata on it. Restore
// deliberately does not call FinalizeRestore itself: the caller controls
// when hardening runs relative to WaitForJob.
func (p *Provider) Restore(ctx context.Context, backup core.Backup, opts core.RestoreOptions) (*core.RestoreJob, error) {
	node := opts.Node
	if node == "" {
		node = backup.Node
	}
	if node == "" {
		return nil, fmt.Errorf("proxmox: restore: no target node given and backup %q has none either", backup.ID)
	}
	if opts.TargetWorkloadID == "" {
		return nil, fmt.Errorf("proxmox: restore: opts.TargetWorkloadID is required")
	}

	form := url.Values{}
	form.Set("vmid", opts.TargetWorkloadID)
	form.Set("archive", backup.ID)
	if opts.Storage != "" {
		form.Set("storage", opts.Storage)
	}
	if opts.Pool != "" {
		// The pool is what a least-privilege token is scoped to: without it,
		// PVE refuses the create for an account that only holds VM.Allocate
		// on /pool/restorelab.
		form.Set("pool", opts.Pool)
	}

	// Override the network in the create call rather than fixing it up
	// afterwards. Two reasons, both learned from a real cluster:
	//
	//   - the workload never exists, not even for an instant, attached to the
	//     bridge it had in production;
	//   - Proxmox validates the restored configuration as it creates it, so a
	//     backup referencing an SDN-managed bridge is refused outright with
	//     403 SDN.Use unless the caller holds that privilege on the production
	//     network - which RestoreLab must never need.
	if opts.Network.Bridge != "" {
		// Every interface the backup carries, not just the first: a workload
		// with two NICs would otherwise be created with its second one still
		// pointing at a production bridge, which Proxmox refuses outright when
		// SDN permissions apply, and which would be a live production bridge
		// where they do not. FinalizeRestore removes the extra ones once the
		// workload exists; this makes sure none of them is ever production.
		for _, iface := range p.backupNetworkDevices(ctx, node, backup.ID) {
			form.Set(iface, renderNetConfig(opts.Network))
		}
	}

	// Stamp ownership at creation for a related reason: a workload that exists
	// without this metadata is one cleanup will refuse to touch, so a failure
	// between creating and hardening would leave an orphan only a human could
	// remove. That happened once; it must not be possible again.
	if desc := renderMetadata(opts.Metadata); desc != "" {
		form.Set("description", desc)
	}
	form.Set("tags", managedTag)

	form.Set("unique", "1")
	form.Set("start", "0")
	if opts.BandwidthKiBps > 0 {
		form.Set("bwlimit", strconv.Itoa(opts.BandwidthKiBps))
	}

	raw, err := p.post(ctx, fmt.Sprintf("/nodes/%s/qemu", node), form)
	if err != nil {
		return nil, err
	}
	var upid string
	if err := json.Unmarshal(raw, &upid); err != nil {
		return nil, fmt.Errorf("proxmox: decode restore task id: %w", err)
	}

	return &core.RestoreJob{
		ID:         upid,
		WorkloadID: opts.TargetWorkloadID,
		Node:       node,
		StartedAt:  time.Now(),
	}, nil
}

// WaitForJob blocks until a Restore job's PVE task settles.
func (p *Provider) WaitForJob(ctx context.Context, job *core.RestoreJob) (*core.TaskState, error) {
	return p.WaitForTask(ctx, job.Node, job.ID)
}

// WaitForTask polls a PVE task (identified by its UPID) every 2s until it
// stops running, honouring ctx cancellation while it waits.
func (p *Provider) WaitForTask(ctx context.Context, node, upid string) (*core.TaskState, error) {
	for {
		raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid), nil)
		if err != nil {
			return nil, err
		}
		var s map[string]any
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("proxmox: decode task status for %s: %w", upid, err)
		}

		if asString(s["status"]) == "stopped" {
			exit := asString(s["exitstatus"])
			ts := &core.TaskState{
				ID:       upid,
				Running:  false,
				Success:  exit == "OK",
				ExitCode: exit,
			}
			if ts.Success {
				ts.Message = "OK"
			} else {
				ts.Message = fmt.Sprintf("proxmox task %s failed: %s", upid, exit)
			}
			return ts, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
}

// FinalizeRestore hardens a just-restored qemu VM. It must be called after
// WaitForJob has reported success for the Restore that created
// opts.TargetWorkloadID; it is not part of Restore itself so that callers
// control exactly when hardening runs (typically: Restore, then
// WaitForJob, then FinalizeRestore).
//
// This is the safety boundary between a raw backup restore and a workload
// RestoreLab is willing to power on: it rewrites every network interface so
// the restored workload can never come up on its original production
// bridge/MAC, disables autostart and delete protection, and stamps
// RestoreLab's ownership metadata (consumed later by Delete's managed-check).
func (p *Provider) FinalizeRestore(ctx context.Context, opts core.RestoreOptions) error {
	node := opts.Node
	if node == "" {
		var err error
		node, _, err = p.resolve(ctx, opts.TargetWorkloadID)
		if err != nil {
			return err
		}
	}
	id := opts.TargetWorkloadID

	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, id), nil)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("proxmox: decode config for %s: %w", id, err)
	}

	form := url.Values{}

	netVal := "virtio,bridge=" + opts.Network.Bridge
	if opts.Network.VLANTag > 0 {
		netVal += fmt.Sprintf(",tag=%d", opts.Network.VLANTag)
	}
	if opts.Network.Firewall {
		netVal += ",firewall=1"
	}
	form.Set("net0", netVal)

	var toDelete []string
	for k := range cfg {
		if netIfaceRE.MatchString(k) && k != "net0" {
			toDelete = append(toDelete, k)
		}
	}
	sort.Strings(toDelete)
	if len(toDelete) > 0 {
		form.Set("delete", strings.Join(toDelete, ","))
	}

	form.Set("onboot", "0")
	form.Set("protection", "0")

	if opts.Name != "" {
		form.Set("name", opts.Name)
	}
	if opts.CPULimit > 0 {
		form.Set("cores", strconv.Itoa(opts.CPULimit))
	}
	if opts.MemoryLimitMB > 0 {
		form.Set("memory", strconv.Itoa(opts.MemoryLimitMB))
	}

	if desc := renderMetadata(opts.Metadata); desc != "" {
		form.Set("description", desc)
	}
	form.Set("tags", managedTag)

	_, err = p.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, id), form)
	return err
}

// Start powers on a workload and waits for the start task to settle.
func (p *Provider) Start(ctx context.Context, id string) error {
	node, kind, err := p.resolve(ctx, id)
	if err != nil {
		return err
	}
	return p.runLifecycleTask(ctx, node, kind, id, "start")
}

// Stop hard-stops a workload (not a graceful shutdown: RestoreLab workloads
// are throwaway, and a hung guest must not block cleanup) and waits for the
// stop task to settle.
func (p *Provider) Stop(ctx context.Context, id string) error {
	node, kind, err := p.resolve(ctx, id)
	if err != nil {
		return err
	}
	return p.runLifecycleTask(ctx, node, kind, id, "stop")
}

func (p *Provider) runLifecycleTask(ctx context.Context, node, kind, id, action string) error {
	raw, err := p.post(ctx, fmt.Sprintf("/nodes/%s/%s/%s/status/%s", node, kind, id, action), nil)
	if err != nil {
		return err
	}
	var upid string
	if err := json.Unmarshal(raw, &upid); err != nil {
		return fmt.Errorf("proxmox: decode %s task id for %s: %w", action, id, err)
	}
	ts, err := p.WaitForTask(ctx, node, upid)
	if err != nil {
		return err
	}
	if !ts.Success {
		return fmt.Errorf("proxmox: %s %s: %s", action, id, ts.Message)
	}
	return nil
}

// isManaged reports whether a workload carries RestoreLab's ownership marks:
// the "restorelab" tag, or "restorelab_managed=true" in its description.
// Delete relies on this as its safety gate.
func (p *Provider) isManaged(ctx context.Context, node, kind, id string) (bool, error) {
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/config", node, kind, id), nil)
	if err != nil {
		return false, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, fmt.Errorf("proxmox: decode config for %s: %w", id, err)
	}

	// The ownership metadata stamped by FinalizeRestore is the proof, not the
	// tag: a tag is trivially copied onto a production VM by a human (or by a
	// clone), and this check is the last gate before a destroy. The tag stays
	// useful for filtering in the UI, never for authorising a delete.
	desc := asString(cfg["description"])
	return strings.Contains(desc, core.MetadataManaged+"=true"), nil
}

// Delete destroys a workload, but only one RestoreLab created. It refuses
// (returning core.ErrNotManaged, without issuing any destructive request)
// when the workload does not carry RestoreLab's ownership metadata.
func (p *Provider) Delete(ctx context.Context, id string) error {
	// Second, independent gate: RestoreLab only ever allocates temporary
	// workloads inside its reserved ID range, so anything outside it cannot be
	// ours whatever its metadata claims. Two gates means a single mistake -
	// a stray tag, a copied description - is not enough to destroy a VM.
	if n, convErr := strconv.Atoi(id); convErr != nil || n < p.cfg.TempIDMin || n > p.cfg.TempIDMax {
		return fmt.Errorf("proxmox: refusing to delete workload %q: outside the reserved temporary id range %d-%d: %w",
			id, p.cfg.TempIDMin, p.cfg.TempIDMax, core.ErrNotManaged)
	}

	node, kind, err := p.resolve(ctx, id)
	if err != nil {
		return err
	}

	managed, err := p.isManaged(ctx, node, kind, id)
	if err != nil {
		return err
	}
	if !managed {
		return fmt.Errorf("proxmox: refusing to delete workload %q: %w", id, core.ErrNotManaged)
	}

	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/status/current", node, kind, id), nil)
	if err != nil {
		return err
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("proxmox: decode status/current for %s: %w", id, err)
	}
	if asString(st["status"]) == "running" {
		if err := p.Stop(ctx, id); err != nil {
			return fmt.Errorf("proxmox: delete %s: stopping before destroy: %w", id, err)
		}
	}

	delRaw, err := p.delete(ctx, fmt.Sprintf("/nodes/%s/%s/%s", node, kind, id), url.Values{
		"purge":                      {"1"},
		"destroy-unreferenced-disks": {"1"},
	})
	if err != nil {
		return err
	}
	var upid string
	if err := json.Unmarshal(delRaw, &upid); err != nil {
		return fmt.Errorf("proxmox: decode delete task id for %s: %w", id, err)
	}
	ts, err := p.WaitForTask(ctx, node, upid)
	if err != nil {
		return err
	}
	if !ts.Success {
		return fmt.Errorf("proxmox: delete %s: %s", id, ts.Message)
	}
	return nil
}

// NodeCapacity reports live resource usage for a single node.
func (p *Provider) NodeCapacity(ctx context.Context, node string) (*core.Node, error) {
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/status", node), nil)
	if err != nil {
		return nil, err
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("proxmox: decode status for node %s: %w", node, err)
	}

	mem, _ := s["memory"].(map[string]any)
	rootfs, _ := s["rootfs"].(map[string]any)
	cpuinfo, _ := s["cpuinfo"].(map[string]any)

	return &core.Node{
		ID:               node,
		Name:             node,
		Online:           true, // a successful response means it is reachable
		CPUCores:         asInt(cpuinfo["cpus"]),
		CPUUsage:         asFloat(s["cpu"]),
		MemoryTotalBytes: asInt64(mem["total"]),
		MemoryUsedBytes:  asInt64(mem["used"]),
		DiskTotalBytes:   asInt64(rootfs["total"]),
		DiskUsedBytes:    asInt64(rootfs["used"]),
	}, nil
}

// ValidateIsolation is a best-effort heuristic, not a network-level
// guarantee: it only checks the PVE bridge's own configuration (it exists,
// carries no bridge_ports and no gateway), not actual reachability from
// that bridge to production. A bridge that is misconfigured downstream (an
// uplink added at the switch, a routed VLAN) can still pass this check.
func (p *Provider) ValidateIsolation(ctx context.Context, node string, network core.NetworkConfig) error {
	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/network", node), nil)
	if err != nil {
		return err
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("proxmox: decode network config for node %s: %w", node, err)
	}

	var bridgesSeen int
	for _, e := range entries {
		if asString(e["type"]) == "bridge" {
			bridgesSeen++
		}
		if asString(e["iface"]) != network.Bridge {
			continue
		}
		ports := strings.TrimSpace(asString(e["bridge_ports"]))
		gateway := strings.TrimSpace(asString(e["gateway"]))
		address := strings.TrimSpace(asString(e["cidr"]))
		if address == "" {
			address = strings.TrimSpace(asString(e["address"]))
		}
		if ports != "" || gateway != "" {
			return fmt.Errorf("proxmox: bridge %q on node %q has an uplink (bridge_ports=%q gateway=%q): %w",
				network.Bridge, node, ports, gateway, core.ErrNetworkNotIsolated)
		}
		_ = address // an address without a gateway does not by itself defeat isolation
		return nil
	}

	// Proxmox does not always show a node's bridges to a non-administrative
	// token: on PVE 9.2.3 a token holding Sys.Audit on the node received only
	// the physical NIC, with ?type=bridge returning an empty list. Reporting
	// that as "not isolated" would block a drill whose bridge is in fact fine,
	// so say what is actually true - nothing could be verified.
	if bridgesSeen == 0 {
		return fmt.Errorf("proxmox: cannot read the bridges of node %q with these credentials (%d interface(s) visible): %w",
			node, len(entries), core.ErrIsolationUnverified)
	}
	return fmt.Errorf("proxmox: bridge %q not found on node %q: %w", network.Bridge, node, core.ErrNetworkNotIsolated)
}

// managedTag marks every workload RestoreLab creates. It is a filtering aid,
// never proof of ownership - see isManaged.
const managedTag = "restorelab"

// renderNetConfig builds a Proxmox network device string pointing at the
// isolated bridge. No MAC address is given, so Proxmox generates a fresh one:
// putting a running production workload's MAC on a second machine would be its
// own kind of incident.
func renderNetConfig(network core.NetworkConfig) string {
	model := network.Model
	if model == "" {
		model = "virtio"
	}
	cfg := model + ",bridge=" + network.Bridge
	if network.VLANTag > 0 {
		cfg += fmt.Sprintf(",tag=%d", network.VLANTag)
	}
	if network.Firewall {
		cfg += ",firewall=1"
	}
	return cfg
}

// renderMetadata renders RestoreLab's ownership metadata as the key=value
// lines stored in a workload's description, sorted so the value is stable.
func renderMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+metadata[k])
	}
	return strings.Join(lines, "\n")
}

// backupNetworkDevices lists the network interfaces stored in a backup, so a
// restore can neutralise all of them. It falls back to just net0 when the
// configuration cannot be read: overriding one interface is what RestoreLab
// did before this existed, so the failure mode is no worse than it was, and a
// workload with more interfaces fails loudly at restore rather than quietly
// coming up attached to production.
func (p *Provider) backupNetworkDevices(ctx context.Context, node, volid string) []string {
	config, err := p.BackupConfig(ctx, node, volid)
	if err != nil {
		return []string{"net0"}
	}
	nets := BackupNetworkDevices(config)
	if len(nets) == 0 {
		return []string{"net0"}
	}
	return nets
}
