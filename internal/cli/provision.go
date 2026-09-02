package cli

// The provisioning sequence, with nothing printed.
//
// `restorelab connect` and the browser's first-run wizard both need it, and
// they must not each have their own: the order of these calls is load-bearing
// - the provider is stored before the token is verified, because Proxmox
// reveals a token secret exactly once and a token that exists on the cluster
// but nowhere in the configuration is the one failure this cannot recover
// from. Two copies of that order would drift, and the drift would only show
// up on somebody's cluster.
//
// So this file holds the sequence, and the two callers hold the presentation:
// connect prints it, the wizard renders it as JSON.

import (
	"context"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/providers"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
)

// provisionOptions is what provisioning needs, whoever asked for it.
type provisionOptions struct {
	Endpoint      string
	AdminUser     string
	AdminPassword string
	Insecure      bool
	CACertPEM     string
	CACertPath    string

	ProviderID  string
	ServiceUser string
	TokenName   string
	RoleName    string
	Pool        string
	Node        string
	Storages    []string

	// CreateBridge asks for the isolated bridge; ApplyBridge false writes the
	// node's network configuration without reloading it.
	CreateBridge bool
	ApplyBridge  bool
	BridgeName   string

	// ReadOnly provisions an account that can discover and dry-run but cannot
	// restore, start or destroy anything. DryRun changes nothing at all.
	//
	// Both exist for `connect`; the wizard never sets them. An installation
	// that ends read-only cannot run the drill it was installed for, and a
	// dry run from a browser would be a wizard that pretends.
	ReadOnly bool
	DryRun   bool

	// AfterVerify runs once the token has been proved to work, while the
	// administrator client is still open. It is how `connect` keeps its
	// interactive bridge prompt without owning the sequence: this function
	// holds the order, the caller holds the conversation.
	//
	// The clients passed in are valid only for the duration of the call.
	AfterVerify func(context.Context, *proxmox.AdminClient, core.HypervisorProvider, []core.Node) error
}

// provisionResult is what provisioning produced.
//
// Steps are populated even when the call fails, because Bootstrap records
// them as it goes: a caller can then show how far it got, and re-running is
// safe because every step is idempotent.
type provisionResult struct {
	Steps  []proxmox.BootstrapStep
	Entry  config.Provider
	Node   string
	Bridge string
	// BridgeApplied is true when the node's network configuration was
	// reloaded rather than only written.
	BridgeApplied bool
	// BridgeCreated is false when the bridge was already there.
	BridgeCreated bool

	// TokenID is the token Proxmox created or reused.
	TokenID string
	// Bootstrap is what was asked of the cluster, so a caller can describe it
	// without rebuilding the options and getting them subtly different.
	Bootstrap proxmox.BootstrapOptions
	// NodeCount is how many nodes the new token could see. Proving that is
	// the only thing that makes the whole exercise worth anything.
	NodeCount int
}

// provision connects a Proxmox cluster and stores its sealed token.
//
// It prints nothing. Every message a human should see is the caller's to
// write, from the result.
func (a *app) provision(ctx context.Context, opts provisionOptions) (*provisionResult, error) {
	out := &provisionResult{}

	// This is often the very first thing that ever runs on a machine, so it
	// must work with no configuration and no master key.
	cfg, key, _, err := a.ensureInitialisedQuietly()
	if err != nil {
		return out, err
	}

	admin, err := proxmox.NewAdminClient(proxmox.AdminConfig{
		Endpoint:           opts.Endpoint,
		Username:           opts.AdminUser,
		Password:           opts.AdminPassword,
		InsecureSkipVerify: opts.Insecure,
		CACertPEM:          opts.CACertPEM,
		Timeout:            30 * time.Second,
	})
	if err != nil {
		return out, err
	}
	defer admin.Close()

	if err := admin.Login(ctx); err != nil {
		return out, err
	}

	// Re-running must reconcile roles and ACLs without destroying a token
	// that already works. Proxmox reveals a secret once, so reusing is only
	// safe when the configuration already holds that exact token's secret.
	reuse := false
	if existing, err := cfg.Provider(opts.ProviderID); err == nil {
		if existing.TokenID == opts.ServiceUser+"!"+opts.TokenName && existing.TokenSecret != "" {
			reuse = true
		}
	}

	bootstrap := proxmox.BootstrapOptions{
		UserID:             opts.ServiceUser,
		Comment:            "RestoreLab recovery drills",
		TokenName:          opts.TokenName,
		RoleName:           opts.RoleName,
		Pool:               opts.Pool,
		ReadOnly:           opts.ReadOnly,
		Node:               opts.Node,
		Storages:           opts.Storages,
		Bridge:             opts.BridgeName,
		DryRun:             opts.DryRun,
		ReuseExistingToken: reuse,
	}
	out.Bootstrap = bootstrap

	result, err := admin.Bootstrap(ctx, bootstrap)
	if result != nil {
		// Kept even on failure: they say how far it got.
		out.Steps = result.Steps
	}
	if err != nil {
		return out, err
	}
	out.TokenID = result.TokenID

	// A dry run created nothing, so there is nothing to seal and nothing to
	// verify. Stopping here is what makes it a dry run.
	if opts.DryRun {
		return out, nil
	}

	// The provider is stored before the token is verified. A token that
	// exists in Proxmox but nowhere in the configuration cannot be recovered,
	// because PVE hands its secret out exactly once.
	entry := config.Provider{
		ID:         opts.ProviderID,
		Kind:       providers.KindProxmox,
		Roles:      []string{providers.RoleHypervisor, providers.RoleBackup},
		Endpoint:   opts.Endpoint,
		TokenID:    result.TokenID,
		Insecure:   opts.Insecure,
		CACertPath: opts.CACertPath,
		Node:       opts.Node,
		Pool:       bootstrap.Pool,
		TempIDMin:  core.DefaultTempIDMin,
		TempIDMax:  core.DefaultTempIDMax,
	}
	previousSecret := ""
	if existing, err := cfg.Provider(entry.ID); err == nil {
		previousSecret = existing.TokenSecret
	}

	cfg.Upsert(entry)
	switch {
	case result.Secret != "":
		if err := cfg.SetProviderSecret(entry.ID, result.Secret, key); err != nil {
			return out, err
		}
	case previousSecret != "":
		// The token was reused: Proxmox gave no secret because it only hands
		// one out at creation. Keep the one already sealed.
		stored, err := cfg.Provider(entry.ID)
		if err != nil {
			return out, err
		}
		stored.TokenSecret = previousSecret
	default:
		return out, fmt.Errorf(
			"no token secret available for %s: delete the token in Proxmox and connect again",
			entry.TokenID)
	}
	if cfg.Defaults.Provider == "" {
		cfg.Defaults.Provider = entry.ID
	}
	if err := config.Save(a.path(), cfg); err != nil {
		return out, fmt.Errorf("the token was created in Proxmox but could not be saved: %w", err)
	}
	a.cfg = cfg
	out.Entry = entry

	// Prove the token works, which is the only thing that makes the exercise
	// worth anything.
	hv, _, err := a.hypervisor(entry.ID)
	if err != nil {
		return out, err
	}
	nodes, err := hv.ListNodes(ctx)
	if err != nil {
		return out, fmt.Errorf("the token was created but cannot be used: %w", err)
	}

	out.NodeCount = len(nodes)

	if opts.AfterVerify != nil {
		if err := opts.AfterVerify(ctx, admin, hv, nodes); err != nil {
			return out, err
		}
	}

	if !opts.CreateBridge {
		return out, nil
	}
	if err := a.provisionBridge(ctx, admin, hv, nodes, opts, out); err != nil {
		return out, err
	}
	return out, nil
}

// provisionBridge creates the isolated bridge, or reports that it is there.
//
// It is the only moment RestoreLab holds the privileges to do it: the service
// token deliberately cannot reconfigure a node's network.
func (a *app) provisionBridge(ctx context.Context, admin *proxmox.AdminClient,
	hv core.HypervisorProvider, nodes []core.Node, opts provisionOptions, out *provisionResult) error {

	node := opts.Node
	if node == "" {
		for _, n := range nodes {
			if n.Online {
				node = n.ID
				break
			}
		}
	}
	if node == "" || opts.BridgeName == "" {
		return fmt.Errorf("no node or bridge to create: node %q, bridge %q", node, opts.BridgeName)
	}
	out.Node = node
	out.Bridge = opts.BridgeName

	// Already there is a success, not an error.
	if validator, ok := hv.(core.NetworkValidator); ok {
		cfg := core.NetworkConfig{Bridge: opts.BridgeName, Isolated: true}
		if err := validator.ValidateIsolation(ctx, node, cfg); err == nil {
			out.Steps = append(out.Steps, proxmox.BootstrapStep{
				Description: "create isolated bridge " + opts.BridgeName + " on " + node,
				Status:      "already exists",
			})
			return nil
		}
	}

	if _, err := admin.EnsureIsolatedBridge(ctx, proxmox.BridgeOptions{
		Node:   node,
		Bridge: opts.BridgeName,
		Apply:  opts.ApplyBridge,
	}); err != nil {
		return fmt.Errorf("creating the isolated bridge %s on %s: %w", opts.BridgeName, node, err)
	}

	status := "created"
	if !opts.ApplyBridge {
		status = "written, applies at the next reboot"
	}
	out.Steps = append(out.Steps, proxmox.BootstrapStep{
		Description: "create isolated bridge " + opts.BridgeName + " on " + node,
		Status:      status,
	})
	out.BridgeCreated = true
	out.BridgeApplied = opts.ApplyBridge
	return nil
}
