package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/crypto"
)

// configEnvVar overrides the config file location.
const configEnvVar = "RESTORELAB_CONFIG"

// defaultConfigRelPath is where DefaultPath looks, relative to the user's
// home directory, when configEnvVar is unset.
const defaultConfigRelPath = ".restorelab/config.yaml"

// ErrNotFound is returned by Load when the config file does not exist, so
// the CLI can point the user at `restorelab init` instead of printing a
// raw filesystem error.
var ErrNotFound = errors.New("config file not found")

// DefaultPath returns $RESTORELAB_CONFIG if set, otherwise
// ~/.restorelab/config.yaml.
func DefaultPath() string {
	if p := os.Getenv(configEnvVar); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No sensible fallback; return the relative path so callers at
		// least get a deterministic, if imperfect, location rather than a
		// hidden error here. Load/Save will surface the real problem when
		// they try to use it.
		return defaultConfigRelPath
	}
	return filepath.Join(home, filepath.FromSlash(defaultConfigRelPath))
}

// Load reads, strictly decodes, defaults and validates the config at path.
// Strict decoding (KnownFields) turns a typo'd YAML key into a load-time
// error instead of a silently ignored field.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	c.applyDefaults()

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// applyDefaults fills in zero-value fields that must not be left at their
// Go zero value. Currently a no-op placeholder: Config's zero value (no
// providers, no networks) is already meaningful, so there is nothing to
// backfill today. Kept as a distinct step so future defaulting logic has an
// obvious home instead of being folded into Load.
func (c *Config) applyDefaults() {}

// Save atomically writes c to path as YAML with owner-only permissions
// (0600), creating parent directories (0700) as needed.
//
// Save refuses to write when any provider's TokenSecret is not a sealed
// value (see crypto.IsSealed): that check is what keeps a plaintext API
// token from ever landing on disk, so it is enforced here rather than left
// to callers to remember.
func Save(path string, c *Config) error {
	for _, p := range c.Providers {
		if p.TokenSecret != "" && !crypto.IsSealed(p.TokenSecret) {
			return fmt.Errorf("refusing to save config: provider %q has an unsealed token_secret (use Config.SetProviderSecret to seal it before saving)", p.ID)
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	// Atomic write: write to a temp file in the same directory (so the
	// final rename is same-filesystem and therefore atomic on POSIX and
	// Windows), then rename over the destination. This avoids ever leaving
	// a half-written config file if the process is interrupted mid-write.
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure the temp file never lingers on any early-return path below.
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp config file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config file %s: %w", path, err)
	}
	cleanup = false
	return nil
}
