package core

import "context"

// Managed metadata keys stamped onto every resource RestoreLab creates.
// Cleanup and delete paths rely on them to prove ownership before destroying
// anything, so they must never be changed without a migration story.
const (
	MetadataManaged       = "restorelab_managed"
	MetadataRecoveryRunID = "restorelab_run_id"
	MetadataSourceID      = "restorelab_source_id"
	MetadataCreatedAt     = "restorelab_created_at"
)

// DefaultTempIDMin and DefaultTempIDMax bound the range a temporary
// workload's id is allocated in when a provider is not configured with its
// own narrower range. This is a safety boundary, not a default in the
// cosmetic sense: it is what makes a mistyped or malicious id outside the
// range refused rather than acted on, both by the API before a provider is
// even asked (the early gate) and by the provider itself when it deletes a
// workload (the independent one). Two copies of these numbers is two chances
// for that gate to quietly narrow or widen without the other noticing, so
// every caller reads them from here.
const (
	DefaultTempIDMin = 9000
	DefaultTempIDMax = 9999
)

// HypervisorProvider is the contract every compute backend implements
// (Proxmox VE today; VMware, Hyper-V, cloud providers later).
//
// Implementations must be safe for concurrent use and must never mutate a
// workload they did not create, apart from read-only calls.
type HypervisorProvider interface {
	// ID is the user-facing identifier of this provider instance ("proxmox-main").
	ID() string
	// Kind is the provider technology ("proxmox").
	Kind() string
	// Ping validates credentials and reachability.
	Ping(ctx context.Context) error

	ListNodes(ctx context.Context) ([]Node, error)
	ListWorkloads(ctx context.Context) ([]Workload, error)
	GetWorkload(ctx context.Context, id string) (*Workload, error)
	GetStatus(ctx context.Context, id string) (*WorkloadStatus, error)

	// AllocateWorkloadID reserves a free identifier for a temporary workload.
	AllocateWorkloadID(ctx context.Context) (string, error)

	// Restore creates a new workload from backup. It never overwrites an
	// existing workload: opts.TargetWorkloadID must be free.
	Restore(ctx context.Context, backup Backup, opts RestoreOptions) (*RestoreJob, error)
	// WaitForJob blocks until an asynchronous provider job settles.
	WaitForJob(ctx context.Context, job *RestoreJob) (*TaskState, error)

	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	// Delete destroys a workload. Implementations must verify RestoreLab
	// ownership metadata first and return ErrNotManaged otherwise.
	Delete(ctx context.Context, id string) error
}

// BackupProvider finds restorable points in time for a workload.
type BackupProvider interface {
	ID() string
	Kind() string
	Ping(ctx context.Context) error

	// ListBackups returns backups for a workload, newest first.
	ListBackups(ctx context.Context, workloadID string) ([]Backup, error)
	// GetLatestBackup returns the most recent backup, or ErrNoBackup.
	GetLatestBackup(ctx context.Context, workloadID string) (*Backup, error)
}

// NetworkValidator is implemented by providers able to prove that a restore
// network is actually isolated from production.
type NetworkValidator interface {
	// ValidateIsolation reports whether the named network can reach production.
	// It returns ErrNetworkNotIsolated when isolation cannot be guaranteed.
	ValidateIsolation(ctx context.Context, node string, network NetworkConfig) error
}

// CapacityReporter is implemented by providers able to answer "can this node
// take one more restore right now?".
type CapacityReporter interface {
	NodeCapacity(ctx context.Context, node string) (*Node, error)
}
