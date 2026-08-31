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
