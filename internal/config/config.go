// Package config reads, validates and persists the on-disk RestoreLab
// configuration: providers (with sealed secrets), network profiles, resource
// limits and defaults. It never handles plaintext provider secrets except
// transiently in memory (see Provider.Secret / Config.SetProviderSecret), and
// it refuses to write a config file that would put a plaintext secret on
// disk.
package config

import (
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Config is the full contents of ~/.restorelab/config.yaml.
type Config struct {
	Version   int                `yaml:"version"`
	Providers []Provider         `yaml:"providers"`
	Networks  map[string]Network `yaml:"networks"`
	Limits    Limits             `yaml:"limits"`
	Defaults  Defaults           `yaml:"defaults"`
	Database  Database           `yaml:"database,omitempty"`
	Scheduler Scheduler          `yaml:"scheduler,omitempty"`
}

// Scheduler governs the drills stored plans queue for themselves.
//
// Every field has a working default, so the block is absent from most
// configurations - and its absence means scheduling is on. An installation
// that upgrades into a version with a scheduler should start honouring the
// schedules its plans already carry.
type Scheduler struct {
	// Enabled is a pointer so that an absent block means enabled while
	// "enabled: false" means off. A plain bool would make the zero value
	// "disabled", and every existing configuration would silently stop
	// drilling the day this field shipped.
	Enabled *bool `yaml:"enabled,omitempty"`

	// GracePeriod is how late a slot may be and still run. Past it the slot
	// is skipped and recorded. Zero means the scheduler's own default.
	GracePeriod time.Duration `yaml:"grace_period,omitempty"`

	// MaxQueueDepth caps the queue the scheduler will add to, so that a
	// dozen plans due at the same minute cannot push the last of them into
	// the working day. Zero means the scheduler's own default.
	MaxQueueDepth int `yaml:"max_queue_depth,omitempty"`
}

// SchedulerEnabled reports whether scheduled drills should run.
func (c *Config) SchedulerEnabled() bool {
	return c.Scheduler.Enabled == nil || *c.Scheduler.Enabled
}

// Database says where the drill history is kept.
//
// An empty URL means the embedded SQLite file in the RestoreLab directory,
// which is the default and needs no configuration at all: history has to work
// without anyone installing anything.
//
// A PostgreSQL URL can carry a password, so it is treated as a secret - never
// rendered in an error, a log, or doctor. Use store.RedactDSN before showing
// it to anyone.
type Database struct {
	URL string `yaml:"url,omitempty"`
}

// Provider is one Proxmox VE or Proxmox Backup Server endpoint RestoreLab can
// talk to.
type Provider struct {
	ID       string   `yaml:"id"`   // "proxmox-main"
	Kind     string   `yaml:"kind"` // "proxmox" | "pbs"
	Roles    []string `yaml:"roles"`
	Endpoint string   `yaml:"endpoint"`
	TokenID  string   `yaml:"token_id"`

	// TokenSecret is ALWAYS sealed on disk (rlsec:v1:...). Save refuses to
	// write a Config whose TokenSecret fields are not sealed; use
	// Config.SetProviderSecret to set it rather than assigning directly.
	TokenSecret string `yaml:"token_secret"`

	Insecure    bool   `yaml:"insecure,omitempty"`
	Fingerprint string `yaml:"fingerprint,omitempty"`
	CACertPath  string `yaml:"ca_cert_path,omitempty"`

	// proxmox-only
	Node          string `yaml:"node,omitempty"`
	BackupStorage string `yaml:"backup_storage,omitempty"`
	// Pool is the resource pool temporary workloads are created in. It is what
	// keeps the service account's destructive rights scoped to the drill area.
	Pool      string `yaml:"pool,omitempty"`
	TempIDMin int    `yaml:"temp_id_min,omitempty"`
	TempIDMax int    `yaml:"temp_id_max,omitempty"`

	// pbs-only
	Datastore  string `yaml:"datastore,omitempty"`
	PVEStorage string `yaml:"pve_storage,omitempty"`
}

// Network is a named network profile that plans reference by name (or by the
// keyword "isolated").
type Network struct {
	Bridge   string `yaml:"bridge"`
	VLANTag  int    `yaml:"vlan_tag,omitempty"`
	Firewall bool   `yaml:"firewall,omitempty"`
	Isolated bool   `yaml:"isolated"`
}

// Limits caps how much a recovery run is allowed to consume, independent of
// what any individual plan asks for.
type Limits struct {
	MaxConcurrentRestores int `yaml:"max_concurrent_restores"`
	MaxRecoveryMemoryMB   int `yaml:"max_recovery_memory_mb"`
	MaxRecoveryDiskGB     int `yaml:"max_recovery_disk_gb"`
}

// Defaults are used by plans and the CLI when a field is left unset.
type Defaults struct {
	Provider       string `yaml:"provider,omitempty"`
	BackupProvider string `yaml:"backup_provider,omitempty"`
	Network        string `yaml:"network,omitempty"` // name into Networks
	Node           string `yaml:"node,omitempty"`
	Storage        string `yaml:"storage,omitempty"`
}

// New returns a conservative starter config: no providers configured yet, an
// isolated network profile ready to use, and modest resource limits.
func New() *Config {
	return &Config{
		Version: 1,
		Networks: map[string]Network{
			"isolated": {
				Bridge:   "vmbr99",
				Isolated: true,
			},
		},
		Limits: Limits{
			MaxConcurrentRestores: 1,
			MaxRecoveryMemoryMB:   16384,
			MaxRecoveryDiskGB:     500,
		},
	}
}

// networkToCore converts a config Network profile into a core.NetworkConfig.
func networkToCore(n Network) core.NetworkConfig {
	return core.NetworkConfig{
		Bridge:   n.Bridge,
		VLANTag:  n.VLANTag,
		Firewall: n.Firewall,
		Isolated: n.Isolated,
	}
}
