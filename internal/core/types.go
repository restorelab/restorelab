// Package core holds RestoreLab's domain model and the interfaces that
// providers and checks implement. It must not import any provider package:
// dependencies always point towards core, never away from it.
package core

import "time"

// WorkloadKind describes the technology behind a workload. Providers map their
// own vocabulary onto these values (Proxmox "qemu"/"lxc", VMware "vm", ...).
type WorkloadKind string

const (
	WorkloadKindVM        WorkloadKind = "vm"
	WorkloadKindContainer WorkloadKind = "container"
	WorkloadKindUnknown   WorkloadKind = "unknown"
)

// PowerState is the coarse run state of a workload, normalised across providers.
type PowerState string

const (
	PowerStateRunning PowerState = "running"
	PowerStateStopped PowerState = "stopped"
	PowerStatePaused  PowerState = "paused"
	PowerStateUnknown PowerState = "unknown"
)

// Node is a physical or virtual host able to run workloads.
type Node struct {
	ID               string
	Name             string
	Cluster          string
	Online           bool
	CPUCores         int
	CPUUsage         float64 // 0..1
	MemoryTotalBytes int64
	MemoryUsedBytes  int64
	DiskTotalBytes   int64
	DiskUsedBytes    int64
}

// MemoryFreeBytes reports how much RAM the node has left, floored at zero.
func (n Node) MemoryFreeBytes() int64 {
	if free := n.MemoryTotalBytes - n.MemoryUsedBytes; free > 0 {
		return free
	}
	return 0
}

// Workload is a VM or a container known to a hypervisor provider.
type Workload struct {
	ID          string // provider-scoped identifier, e.g. "101"
	Name        string
	Kind        WorkloadKind
	Node        string
	Cluster     string
	Tags        []string
	CPUCores    int
	MemoryBytes int64
	DiskBytes   int64
	PowerState  PowerState
	Template    bool

	// Managed is true when RestoreLab created the workload itself. Destructive
	// operations must refuse to touch a workload with Managed == false.
	Managed bool

	// RecoveryRunID links a managed workload back to the run that created it.
	RecoveryRunID string
}

// VerificationState mirrors the backup verification result reported by the
// backup provider, when it exposes one.
type VerificationState string

const (
	VerificationOK      VerificationState = "ok"
	VerificationFailed  VerificationState = "failed"
	VerificationNone    VerificationState = "none"
	VerificationUnknown VerificationState = "unknown"
)

// Backup is a single restorable point in time for a workload.
type Backup struct {
	ID         string // provider-scoped identifier (Proxmox volid, PBS snapshot path, ...)
	WorkloadID string
	ProviderID string
	Datastore  string // storage / datastore holding the backup
	Node       string // node the backup is reachable from, when it matters
	CreatedAt  time.Time
	SizeBytes  int64
	Protected  bool
	Encrypted  bool
	Verified   VerificationState
	Format     string
	Notes      string
}

// Age returns how old the backup is relative to now.
func (b Backup) Age() time.Duration { return time.Since(b.CreatedAt) }

// NetworkConfig describes the network a restored workload is attached to.
// Isolated must stay the default everywhere: a restored production workload
// must never land on a production network.
type NetworkConfig struct {
	Bridge   string
	Model    string
	VLANTag  int
	Firewall bool
	Isolated bool
}

// RestoreOptions carries everything a provider needs to materialise a backup
// into a brand new temporary workload.
type RestoreOptions struct {
	TargetWorkloadID string // temporary ID allocated by RestoreLab
	Name             string
	Node             string
	Storage          string // target storage for restored disks; empty means provider default
	Network          NetworkConfig
	CPULimit         int
	MemoryLimitMB    int
	BandwidthKiBps   int
	Start            bool              // start the workload as part of the restore
	Metadata         map[string]string // stamped onto the workload (restorelab_managed, run id, ...)
}

// RestoreJob is a handle on a restore running asynchronously on the provider.
type RestoreJob struct {
	ID         string // provider task identifier (Proxmox UPID, ...)
	WorkloadID string
	Node       string
	StartedAt  time.Time
}

// TaskState is the normalised state of an asynchronous provider task.
type TaskState struct {
	ID       string
	Running  bool
	Success  bool
	ExitCode string
	Message  string
}

// WorkloadStatus is the live state of a workload.
type WorkloadStatus struct {
	ID          string
	PowerState  PowerState
	Uptime      time.Duration
	AgentReady  bool // guest agent responding
	IPs         []string
	CPUUsage    float64
	MemoryBytes int64
}

// PrimaryIP returns the first usable IP address, preferring IPv4.
func (s WorkloadStatus) PrimaryIP() string {
	for _, ip := range s.IPs {
		if !isIPv6(ip) {
			return ip
		}
	}
	if len(s.IPs) > 0 {
		return s.IPs[0]
	}
	return ""
}

func isIPv6(ip string) bool {
	for i := 0; i < len(ip); i++ {
		if ip[i] == ':' {
			return true
		}
	}
	return false
}
