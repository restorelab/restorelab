package config

import (
	"fmt"
	"net/url"
	"strings"
)

// validKinds enumerates the provider kinds RestoreLab understands.
var validKinds = map[string]bool{"proxmox": true, "pbs": true}

// validRoles enumerates the roles a provider may declare.
var validRoles = map[string]bool{"hypervisor": true, "backup": true}

// Validate checks c for every structural and safety problem it can find and
// reports them all together, rather than stopping at the first one, so a
// user fixing a config file does not have to re-run validation once per
// mistake.
func (c *Config) Validate() error {
	var errs []string

	seenIDs := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.ID == "" {
			errs = append(errs, fmt.Sprintf("providers[%d]: id is required", i))
		} else if seenIDs[p.ID] {
			errs = append(errs, fmt.Sprintf("providers[%d]: duplicate provider id %q", i, p.ID))
		} else {
			seenIDs[p.ID] = true
		}

		if !validKinds[p.Kind] {
			errs = append(errs, fmt.Sprintf("providers[%d] (%s): kind %q is not supported (proxmox, pbs)", i, p.ID, p.Kind))
		}

		for _, r := range p.Roles {
			if !validRoles[r] {
				errs = append(errs, fmt.Sprintf("providers[%d] (%s): role %q is not supported (hypervisor, backup)", i, p.ID, r))
			}
		}

		if p.Endpoint == "" {
			errs = append(errs, fmt.Sprintf("providers[%d] (%s): endpoint is required", i, p.ID))
		} else if u, err := url.Parse(p.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Sprintf("providers[%d] (%s): endpoint %q is not a valid URL", i, p.ID, p.Endpoint))
		} else if u.Scheme != "https" && u.Scheme != "http" {
			errs = append(errs, fmt.Sprintf("providers[%d] (%s): endpoint scheme %q must be http or https", i, p.ID, u.Scheme))
		}
		// Plain http is a warning, not a validation failure: some labs run
		// Proxmox behind a trusted internal network without TLS. We do not
		// have a logger here, so callers that care should check the scheme
		// themselves (Provider.Endpoint) and warn in their own UI layer.

		if p.Kind == "proxmox" && p.TempIDMin != 0 && p.TempIDMax != 0 && p.TempIDMin >= p.TempIDMax {
			errs = append(errs, fmt.Sprintf("providers[%d] (%s): temp_id_min must be less than temp_id_max", i, p.ID))
		}
	}

	if c.Defaults.Network != "" {
		n, ok := c.Networks[c.Defaults.Network]
		if !ok {
			errs = append(errs, fmt.Sprintf("defaults.network %q does not name a network profile in networks", c.Defaults.Network))
		} else if !n.Isolated {
			// Production-network restores must be an explicit, per-plan
			// opt-in (a plan can still set restore.network to a non-isolated
			// profile by name). Letting a non-isolated profile become the
			// silent default would mean a plan that forgets to set
			// restore.network at all could land a restored workload on a
			// production network without anyone asking for that.
			errs = append(errs, fmt.Sprintf("defaults.network %q is not marked isolated: true; a non-isolated network must never be the default, only an explicit per-plan choice", c.Defaults.Network))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
