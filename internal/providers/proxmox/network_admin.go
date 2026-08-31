package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// ---------------------------------------------------------------------------
// Isolated bridge provisioning.
//
// RestoreLab restores production workloads onto an isolated bridge: a Linux
// bridge with no physical ports and no gateway, so a restored clone cannot
// reach anything beyond itself, no matter how well-networked its guest OS
// thinks it is. Today an administrator does this by hand, SSHing into the
// node and editing /etc/network/interfaces. EnsureIsolatedBridge does the
// same thing through the API, while the administrator's own credentials are
// already in hand (during Bootstrap/connect) - removing the last manual step
// of onboarding. It lives on AdminClient, not Provider: the day-to-day
// service token must never hold the privilege to rewrite a node's network
// configuration, which is exactly why this file exists here and not there.
// ---------------------------------------------------------------------------

// bridgeNamePattern matches what Proxmox itself expects from a Linux bridge
// interface name: "vmbr" followed by 1-4 digits. The upper bound (4094) is
// checked separately since a regex alone would also accept e.g. "vmbr9999".
var bridgeNamePattern = regexp.MustCompile(`^vmbr([0-9]{1,4})$`)

// maxBridgeNumber is the highest vmbrN Proxmox accepts.
const maxBridgeNumber = 4094

// validateBridgeName rejects anything that is not a well-formed "vmbrN"
// name, 0 <= N <= 4094. A typo here (br0, vmbr-1, vmbr99999) would otherwise
// either be rejected confusingly deep inside the PVE API or, worse, quietly
// accepted as some other kind of interface - so the check and a clear
// explanation of the expected form live here, before any request is sent.
func validateBridgeName(name string) error {
	invalid := func() error {
		return fmt.Errorf(
			"proxmox: bridge name %q is invalid: expected the form \"vmbrN\" with N between 0 and %d (e.g. \"vmbr99\")",
			name, maxBridgeNumber,
		)
	}
	m := bridgeNamePattern.FindStringSubmatch(name)
	if m == nil {
		return invalid()
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n > maxBridgeNumber {
		return invalid()
	}
	return nil
}

// defaultBridgeComment marks a bridge as RestoreLab's own, so an
// administrator inspecting the node's network configuration later
// understands what it is and why it has no ports.
const defaultBridgeComment = "RestoreLab isolated recovery network"

// BridgeOptions describes the isolated bridge to provision.
type BridgeOptions struct {
	Node    string // required
	Bridge  string // required, e.g. "vmbr99"
	Comment string // defaults to defaultBridgeComment
	Apply   bool   // apply the pending network configuration immediately
	DryRun  bool
}

func (o BridgeOptions) validate() error {
	if o.Node == "" {
		return errors.New("proxmox: BridgeOptions.Node is required")
	}
	if o.Bridge == "" {
		return errors.New("proxmox: BridgeOptions.Bridge is required")
	}
	return validateBridgeName(o.Bridge)
}

// BridgeResult is what EnsureIsolatedBridge produced.
type BridgeResult struct {
	Steps   []BootstrapStep
	Applied bool
	// PendingApply is true when the bridge was written but not activated:
	// it exists in the node's pending configuration and takes effect on the
	// next reboot or apply.
	PendingApply bool
}

// EnsureIsolatedBridge idempotently provisions the isolated bridge a
// restored workload's NIC is reattached to: a Linux bridge with no ports and
// no address/gateway, so nothing plugged into it can reach the rest of the
// network. It requires a prior successful Login. Every step is recorded in
// BridgeResult.Steps in the order performed, even when a later step fails -
// so a caller can show partial progress on error.
//
// Honest caveat about Apply: activating a node's pending network
// configuration (PUT /nodes/{node}/network) reloads that node's entire
// networking stack, not just the interface just added. Creating a brand new,
// portless bridge does not itself touch any existing interface's
// configuration, so in practice this is safe - but "in practice" is not a
// guarantee on someone else's hypervisor, and a reload is felt cluster-wide
// by anything depending on that node's network at that instant. The caller
// is expected to have obtained explicit confirmation from whoever operates
// the node before passing Apply: true; EnsureIsolatedBridge itself does not
// ask, and DryRun does not apply anything regardless of Apply.
func (c *AdminClient) EnsureIsolatedBridge(ctx context.Context, opts BridgeOptions) (*BridgeResult, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if !c.loggedIn() {
		return nil, errors.New("proxmox: EnsureIsolatedBridge requires a successful Login first")
	}

	comment := opts.Comment
	if comment == "" {
		comment = defaultBridgeComment
	}

	result := &BridgeResult{}
	record := func(step BootstrapStep) {
		if step.Description != "" {
			result.Steps = append(result.Steps, step)
		}
	}

	createDesc := fmt.Sprintf("create isolated bridge %s on %s", opts.Bridge, opts.Node)
	applyDesc := fmt.Sprintf("apply pending network configuration on %s", opts.Node)

	existing, err := c.findNetworkInterface(ctx, opts.Node, opts.Bridge)
	if err != nil {
		return result, fmt.Errorf("proxmox: list network interfaces on %s: %w", opts.Node, err)
	}

	if existing != nil {
		// checkSafeToReuse is the single most important call in this file:
		// a bridge that already carries ports or an address is attached to
		// something real, and quietly repurposing it as "isolated" would
		// cut the node off the network rather than protect a restored
		// clone. Refuse loudly instead of guessing.
		if err := checkSafeToReuse(opts.Bridge, existing); err != nil {
			return result, err
		}
		record(BootstrapStep{
			Description: createDesc,
			Status:      "already exists",
			Detail:      fmt.Sprintf("%s is already an isolated bridge: no ports, no address or gateway configured", opts.Bridge),
		})
		// An interface only shows up in this listing once it is part of the
		// node's configuration; nothing pending is left to apply.
		return result, nil
	}

	if opts.DryRun {
		record(BootstrapStep{Description: createDesc, Status: "would create"})
		if opts.Apply {
			record(BootstrapStep{Description: applyDesc, Status: "would create"})
		} else {
			result.PendingApply = true
			record(BootstrapStep{
				Description: applyDesc,
				Status:      "would create",
				Detail:      fmt.Sprintf("%s would exist only in %s's pending network configuration; it would take effect on the next reboot or apply", opts.Bridge, opts.Node),
			})
		}
		return result, nil
	}

	if err := c.createIsolatedBridge(ctx, opts.Node, opts.Bridge, comment); err != nil {
		return result, err
	}
	record(BootstrapStep{Description: createDesc, Status: "created"})

	if !opts.Apply {
		result.PendingApply = true
		record(BootstrapStep{
			Description: applyDesc,
			Status:      "skipped",
			Detail: fmt.Sprintf(
				"%s was created but exists only in %s's pending network configuration; it needs an apply (EnsureIsolatedBridge with Apply: true) or a reboot to become active",
				opts.Bridge, opts.Node,
			),
		})
		return result, nil
	}

	if err := c.applyNetwork(ctx, opts.Node); err != nil {
		return result, err
	}
	result.Applied = true
	record(BootstrapStep{Description: applyDesc, Status: "created"})
	return result, nil
}

// RevertPendingNetwork discards a node's pending network changes via
// DELETE /nodes/{node}/network, so a caller can back out after a failed or
// unwanted apply instead of leaving the node in a half-configured state.
func (c *AdminClient) RevertPendingNetwork(ctx context.Context, node string) error {
	if node == "" {
		return errors.New("proxmox: RevertPendingNetwork requires a node")
	}
	if !c.loggedIn() {
		return errors.New("proxmox: RevertPendingNetwork requires a successful Login first")
	}
	if _, err := c.doRequest(ctx, http.MethodDelete, "/nodes/"+node+"/network", nil); err != nil {
		return fmt.Errorf("proxmox: revert pending network configuration on %s: %w", node, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// networkInterface is the subset of GET /nodes/{node}/network fields
// EnsureIsolatedBridge needs to decide whether a pre-existing interface is
// safely reusable as an isolated bridge.
type networkInterface struct {
	Type        string
	BridgePorts string
	Address     string
	Gateway     string
	CIDR        string
	Address6    string
	Gateway6    string
	CIDR6       string
}

// findNetworkInterface looks up one interface by name via
// GET /nodes/{node}/network, returning nil (not an error) when it does not
// exist - that is the expected, common case this function is called for.
func (c *AdminClient) findNetworkInterface(ctx context.Context, node, iface string) (*networkInterface, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/nodes/"+node+"/network", nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, fmt.Errorf("decode network interfaces: %w", err)
	}
	for _, row := range rows {
		if asString(row["iface"]) != iface {
			continue
		}
		return &networkInterface{
			Type:        asString(row["type"]),
			BridgePorts: asString(row["bridge_ports"]),
			Address:     asString(row["address"]),
			Gateway:     asString(row["gateway"]),
			CIDR:        asString(row["cidr"]),
			Address6:    asString(row["address6"]),
			Gateway6:    asString(row["gateway6"]),
			CIDR6:       asString(row["cidr6"]),
		}, nil
	}
	return nil, nil
}

// checkSafeToReuse decides whether a pre-existing interface with the
// requested bridge name is, in fact, an isolated bridge we can treat as
// already provisioned - or something real that a wrong Bridge name would
// otherwise quietly turn into an isolated one, severing whatever currently
// depends on it. IPv6 fields are checked alongside their IPv4 counterparts
// for the same reason: an address or gateway on either means something
// already relies on this interface.
func checkSafeToReuse(name string, existing *networkInterface) error {
	if existing.Type != "bridge" {
		return fmt.Errorf("proxmox: interface %q already exists but is type %q, not a bridge; refusing to touch it", name, existing.Type)
	}
	if ports := existing.BridgePorts; ports != "" && ports != "none" {
		return fmt.Errorf(
			"proxmox: bridge %q already exists with bridge_ports %q attached; refusing to turn it into an isolated bridge, since that would sever whatever is attached to it",
			name, ports,
		)
	}
	if existing.Address != "" || existing.CIDR != "" || existing.Address6 != "" || existing.CIDR6 != "" {
		return fmt.Errorf("proxmox: bridge %q already exists with an address configured; refusing to touch it, since that means something already relies on it", name)
	}
	if existing.Gateway != "" || existing.Gateway6 != "" {
		return fmt.Errorf("proxmox: bridge %q already exists with a gateway configured; refusing to touch it, since that means something already relies on it", name)
	}
	return nil
}

// createIsolatedBridge issues the POST that creates a portless, gatewayless
// Linux bridge. bridge_ports is sent explicitly empty (as opposed to
// omitted) so PVE never falls back to an implicit default: the created
// bridge is unambiguously portless from the moment it exists.
func (c *AdminClient) createIsolatedBridge(ctx context.Context, node, iface, comment string) error {
	if _, err := c.doRequest(ctx, http.MethodPost, "/nodes/"+node+"/network", url.Values{
		"iface":        {iface},
		"type":         {"bridge"},
		"autostart":    {"1"},
		"bridge_ports": {""},
		"bridge_stp":   {"off"},
		"bridge_fd":    {"0"},
		"comments":     {comment},
	}); err != nil {
		return fmt.Errorf("proxmox: create bridge %s on %s: %w", iface, node, err)
	}
	return nil
}

// applyNetwork issues PUT /nodes/{node}/network, activating every pending
// network change on the node - not just the bridge just added. See the
// caveat documented on EnsureIsolatedBridge.
func (c *AdminClient) applyNetwork(ctx context.Context, node string) error {
	if _, err := c.doRequest(ctx, http.MethodPut, "/nodes/"+node+"/network", nil); err != nil {
		return fmt.Errorf("proxmox: apply network configuration on %s: %w", node, err)
	}
	return nil
}
