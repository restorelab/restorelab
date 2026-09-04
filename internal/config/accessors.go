package config

import (
	"fmt"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
)

// Provider looks up a provider by ID.
func (c *Config) Provider(id string) (*Provider, error) {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i], nil
		}
	}
	return nil, fmt.Errorf("no provider %q in config", id)
}

// ProvidersByRole returns every provider that declares role among its Roles.
func (c *Config) ProvidersByRole(role string) []Provider {
	var out []Provider
	for _, p := range c.Providers {
		for _, r := range p.Roles {
			if r == role {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Network looks up a named network profile.
func (c *Config) Network(name string) (Network, error) {
	n, ok := c.Networks[name]
	if !ok {
		return Network{}, fmt.Errorf("no network profile %q in config", name)
	}
	return n, nil
}

// ResolveNetwork resolves a plan-level network reference into a
// core.NetworkConfig. The name "isolated" is not special-cased against a
// hardcoded bridge: it resolves to whatever network profile is named
// "isolated" in the config, same as any other name, which is why New()
// always seeds one.
func (c *Config) ResolveNetwork(name string) (core.NetworkConfig, error) {
	n, err := c.Network(name)
	if err != nil {
		return core.NetworkConfig{}, err
	}
	return networkToCore(n), nil
}

// Upsert replaces the provider with a matching ID, or appends p if none is
// found.
func (c *Config) Upsert(p Provider) {
	for i := range c.Providers {
		if c.Providers[i].ID == p.ID {
			c.Providers[i] = p
			return
		}
	}
	c.Providers = append(c.Providers, p)
}

// Secret decrypts and returns the provider's plaintext API token secret.
//
// It deliberately refuses to return TokenSecret as-is when it is not a
// sealed (rlsec:v1:...) value: a plaintext value in that field means the
// provider was added by code that bypassed SetProviderSecret (or an old,
// pre-sealing config was hand-edited), and silently accepting it would
// defeat the point of sealing. Re-adding the provider through
// SetProviderSecret is the supported way to fix this.
func (p *Provider) Secret(k crypto.Key) (string, error) {
	if !crypto.IsSealed(p.TokenSecret) {
		return "", fmt.Errorf("provider %q has an unsealed token_secret; re-add it (e.g. `restorelab provider add`) so it is sealed with the current master key", p.ID)
	}
	secret, err := crypto.Open(k, p.TokenSecret)
	if err != nil {
		return "", fmt.Errorf("provider %q: %w", p.ID, err)
	}
	return secret, nil
}

// SetProviderSecret seals plaintext under k and stores it as the named
// provider's TokenSecret. The provider must already exist (added via
// Upsert) - silently creating one here from just an ID and a secret would
// leave every other Provider field zero-valued, which is more likely a bug
// than intent.
func (c *Config) SetProviderSecret(id string, plaintext string, k crypto.Key) error {
	p, err := c.Provider(id)
	if err != nil {
		return err
	}
	sealed, err := crypto.Seal(k, plaintext)
	if err != nil {
		return fmt.Errorf("seal secret for provider %q: %w", id, err)
	}
	p.TokenSecret = sealed
	return nil
}

// Redacted returns a copy of p with TokenSecret replaced by a fixed
// placeholder, safe to print, log, or serialise for debugging.
func (p Provider) Redacted() Provider {
	if p.TokenSecret != "" {
		p.TokenSecret = "***"
	}
	return p
}

// String implements fmt.Stringer using the redacted form, so an accidental
// %v/%s of a Provider (in a log line, an error, a debug print) never leaks
// the sealed-or-worse secret value.
func (p Provider) String() string {
	r := p.Redacted()
	return fmt.Sprintf("Provider{ID:%s Kind:%s Roles:%v Endpoint:%s TokenID:%s TokenSecret:%s}",
		r.ID, r.Kind, r.Roles, r.Endpoint, r.TokenID, r.TokenSecret)
}

// Notification looks up a notification channel by ID.
func (c *Config) Notification(id string) (*Notification, error) {
	for i := range c.Notifications {
		if c.Notifications[i].ID == id {
			return &c.Notifications[i], nil
		}
	}
	return nil, fmt.Errorf("no notification channel %q in config", id)
}

// UpsertNotification replaces the channel with a matching ID, or appends n if
// none is found. It mirrors Upsert for providers.
func (c *Config) UpsertNotification(n Notification) {
	for i := range c.Notifications {
		if c.Notifications[i].ID == n.ID {
			c.Notifications[i] = n
			return
		}
	}
	c.Notifications = append(c.Notifications, n)
}

// RemoveNotification deletes the channel with the given ID.
//
// An unknown ID is an error rather than a silent no-op: somebody removing a
// channel is trying to stop messages going somewhere, and reporting success
// when nothing was removed would leave them believing they had.
func (c *Config) RemoveNotification(id string) error {
	for i := range c.Notifications {
		if c.Notifications[i].ID == id {
			c.Notifications = append(c.Notifications[:i], c.Notifications[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no notification channel %q in config", id)
}

// SetNotificationURL seals plaintext under k and stores it as the named
// channel's URL. The channel must already exist (added via
// UpsertNotification): creating one here from an ID and a URL alone would
// leave Kind empty, which is more likely a bug than intent.
func (c *Config) SetNotificationURL(id string, plaintext string, k crypto.Key) error {
	n, err := c.Notification(id)
	if err != nil {
		return err
	}
	// Checked here rather than left to Validate, because this is the last
	// moment the value is readable. See ValidateNotificationURL.
	if err := ValidateNotificationURL(plaintext); err != nil {
		return fmt.Errorf("notification channel %q: %w", id, err)
	}
	sealed, err := crypto.Seal(k, plaintext)
	if err != nil {
		return fmt.Errorf("seal url for notification channel %q: %w", id, err)
	}
	n.URL = sealed
	return nil
}

// Target decrypts and returns the channel's plaintext webhook URL.
//
// Like Provider.Secret, it deliberately refuses to return URL as-is when it
// is not a sealed (rlsec:v1:...) value: a plaintext URL in that field means
// the channel was written by code that bypassed SetNotificationURL, or that a
// config file was hand-edited, and quietly accepting it would make the
// sealing optional in practice. Re-adding the channel through
// SetNotificationURL is the supported way to fix this.
func (n *Notification) Target(k crypto.Key) (string, error) {
	if !crypto.IsSealed(n.URL) {
		return "", fmt.Errorf("notification channel %q has an unsealed url; re-add it (e.g. `restorelab notify add`) so it is sealed with the current master key", n.ID)
	}
	target, err := crypto.Open(k, n.URL)
	if err != nil {
		return "", fmt.Errorf("notification channel %q: %w", n.ID, err)
	}
	return target, nil
}

// On reports whether the channel should receive messages. An absent enabled
// field means yes; see the comment on Notification.Enabled.
func (n Notification) On() bool {
	return n.Enabled == nil || *n.Enabled
}

// Redacted returns a copy of n with URL replaced by a fixed placeholder, safe
// to print, log, or serialise for debugging.
//
// The whole URL goes, not just its query: for a Discord webhook the path is
// the credential, so a truncation that keeps the path would redact nothing at
// all. The only safe redaction of a bearer URL is its absence.
func (n Notification) Redacted() Notification {
	if n.URL != "" {
		n.URL = "***"
	}
	return n
}

// String implements fmt.Stringer using the redacted form, so an accidental
// %v/%s of a Notification (in a log line, an error, a debug print, or a %v of
// a whole Config) never leaks the webhook URL.
func (n Notification) String() string {
	r := n.Redacted()
	return fmt.Sprintf("Notification{ID:%s Kind:%s URL:%s Enabled:%t}",
		r.ID, r.Kind, r.URL, n.On())
}
