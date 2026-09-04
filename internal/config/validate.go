package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/restorelab/restorelab/internal/crypto"
)

// validKinds enumerates the provider kinds RestoreLab understands.
var validKinds = map[string]bool{"proxmox": true, "pbs": true}

// validRoles enumerates the roles a provider may declare.
var validRoles = map[string]bool{"hypervisor": true, "backup": true}

// notificationKinds enumerates the channel kinds RestoreLab can render for.
// It is a slice rather than a map so the error message that lists them comes
// out in a stable order: a validation message that reshuffles itself between
// runs looks like a different problem each time.
var notificationKinds = []string{"discord", "slack", "webhook"}

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

	seenChannels := make(map[string]bool, len(c.Notifications))
	for i, n := range c.Notifications {
		if n.ID == "" {
			errs = append(errs, fmt.Sprintf("notifications[%d]: id is required", i))
		} else if seenChannels[n.ID] {
			errs = append(errs, fmt.Sprintf("notifications[%d]: duplicate notification id %q", i, n.ID))
		} else {
			seenChannels[n.ID] = true
		}

		if !isNotificationKind(n.Kind) {
			errs = append(errs, fmt.Sprintf("notifications[%d] (%s): kind %q is not supported (%s)",
				i, n.ID, n.Kind, strings.Join(notificationKinds, ", ")))
		}

		if n.URL == "" {
			errs = append(errs, fmt.Sprintf("notifications[%d] (%s): url is required", i, n.ID))
			continue
		}

		// A sealed url is opaque here: this package validates shape, and it
		// has no master key to open one with. The scheme check below therefore
		// only applies to a url that is still readable, which is the one case
		// where it can help - the moment somebody types it in, before it is
		// sealed and written.
		if strings.HasPrefix(n.URL, "rlsec:") {
			continue
		}

		if err := ValidateNotificationURL(n.URL); err != nil {
			errs = append(errs, fmt.Sprintf("notifications[%d] (%s): %v", i, n.ID, err))
		}
	}

	if c.Server.BaseURL != "" {
		// base_url is pasted into a message a human will click, so a value
		// that is not a whole absolute URL produces a dead link in somebody
		// else's chat client, where nobody can see why.
		if u, err := url.Parse(c.Server.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Sprintf("server.base_url %q is not an absolute URL (it needs a scheme and a host, e.g. https://restorelab.example.com)", c.Server.BaseURL))
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

// ValidateNotificationURL checks a webhook URL while it is still readable.
//
// It is exported and called from two places on purpose: from Validate, and
// from SetNotificationURL just before the value is sealed. The second call is
// the one that matters. Once a URL is sealed it is opaque to this package,
// which holds no master key, so a check that ran only from Validate would
// never fire in the order callers actually work in - set the URL, validate,
// save - because by then the value is already ciphertext. Putting it at the
// single door every plaintext URL comes through means neither the CLI nor the
// API can forget it.
func ValidateNotificationURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	// A sealed value has already been through here once, on its way in.
	if crypto.IsSealed(raw) {
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("url is not a valid URL")
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		// Unlike a provider endpoint, where plain http is only a warning
		// because some labs run Proxmox on a trusted internal network, this
		// one is a hard failure. A webhook url is a bearer credential carried
		// in the request line itself: posting it over plain http hands that
		// credential to every hop on the path, and whoever picks it up can
		// post into the channel forever. There is no second factor to fall
		// back on, and nothing to revoke short of deleting the webhook at the
		// far end.
		//
		// Loopback is the exception, because nothing on a wire can read it,
		// and it is how somebody tries a receiver of their own before
		// pointing a channel at the real thing.
		return fmt.Errorf("url scheme %q must be https (http is only allowed to a loopback address, "+
			"because a webhook url is a bearer credential)", u.Scheme)
	}
	return nil
}

// isNotificationKind reports whether kind is one RestoreLab can render for.
func isNotificationKind(kind string) bool {
	for _, k := range notificationKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// isLoopback reports whether host names this machine, and only this machine.
// "localhost" is matched by name as well as by address because that is what
// people actually type, and resolving it here would make validation depend on
// the resolver of whoever runs it.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
