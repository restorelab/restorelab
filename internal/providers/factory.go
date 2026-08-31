// Package providers turns configuration entries into live provider clients.
// It is the only place that knows which provider kinds exist, so the CLI, the
// engine and the future API never import a concrete provider package.
package providers

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers/pbs"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
)

// Roles a provider entry can declare.
const (
	RoleHypervisor = "hypervisor"
	RoleBackup     = "backup"
)

// Kinds currently implemented.
const (
	KindProxmox = "proxmox"
	KindPBS     = "pbs"
)

// defaultTimeout applies to individual API calls, not to a restore: long
// running operations are tracked as provider tasks and polled.
const defaultTimeout = 30 * time.Second

// NewHypervisor builds the compute provider described by p.
func NewHypervisor(p config.Provider, key crypto.Key) (core.HypervisorProvider, error) {
	switch p.Kind {
	case KindProxmox:
		return newProxmox(p, key)
	case KindPBS:
		return nil, fmt.Errorf("provider %q is a backup server: it cannot restore or start workloads", p.ID)
	default:
		return nil, fmt.Errorf("unknown provider kind %q for provider %q", p.Kind, p.ID)
	}
}

// NewBackup builds the backup provider described by p. Proxmox VE doubles as a
// backup provider: it can list the backups on any storage it has attached,
// which is the simplest path for a PBS that is already added to the cluster.
func NewBackup(p config.Provider, key crypto.Key) (core.BackupProvider, error) {
	switch p.Kind {
	case KindProxmox:
		return newProxmox(p, key)
	case KindPBS:
		return newPBS(p, key)
	default:
		return nil, fmt.Errorf("unknown provider kind %q for provider %q", p.Kind, p.ID)
	}
}

func newProxmox(p config.Provider, key crypto.Key) (*proxmox.Provider, error) {
	secret, err := p.Secret(key)
	if err != nil {
		return nil, err
	}
	ca, err := readCA(p)
	if err != nil {
		return nil, err
	}
	return proxmox.New(proxmox.Config{
		ID:                 p.ID,
		Endpoint:           p.Endpoint,
		TokenID:            p.TokenID,
		TokenSecret:        secret,
		InsecureSkipVerify: p.Insecure,
		CACertPEM:          ca,
		Timeout:            defaultTimeout,
		BackupStorage:      p.BackupStorage,
		TempIDMin:          p.TempIDMin,
		TempIDMax:          p.TempIDMax,
	})
}

func newPBS(p config.Provider, key crypto.Key) (*pbs.Provider, error) {
	secret, err := p.Secret(key)
	if err != nil {
		return nil, err
	}
	ca, err := readCA(p)
	if err != nil {
		return nil, err
	}
	return pbs.New(pbs.Config{
		ID:                 p.ID,
		Endpoint:           p.Endpoint,
		TokenID:            p.TokenID,
		TokenSecret:        secret,
		Datastore:          p.Datastore,
		PVEStorage:         p.PVEStorage,
		InsecureSkipVerify: p.Insecure,
		CACertPEM:          ca,
		Fingerprint:        p.Fingerprint,
		Timeout:            defaultTimeout,
	})
}

// Pinger is the smallest useful capability set, shared by both provider
// interfaces, so a "test this provider" command does not need to know which
// kind it is holding.
type Pinger interface {
	ID() string
	Kind() string
	Ping(ctx context.Context) error
}

func readCA(p config.Provider) (string, error) {
	if p.CACertPath == "" {
		return "", nil
	}
	pem, err := os.ReadFile(p.CACertPath)
	if err != nil {
		return "", fmt.Errorf("provider %q: read ca_cert_path: %w", p.ID, err)
	}
	return string(pem), nil
}

// HasRole reports whether the provider entry declares the given role.
// A provider with no roles declared is assumed to be capable of everything its
// kind supports, which keeps hand-written configs forgiving.
func HasRole(p config.Provider, role string) bool {
	if len(p.Roles) == 0 {
		return role == RoleBackup || p.Kind == KindProxmox
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
