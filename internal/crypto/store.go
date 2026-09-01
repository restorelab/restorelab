package crypto

import (
	"fmt"
	"os"
	"path/filepath"
)

// masterKeyEnvVar is checked first by LoadKey, so a key can be injected by a
// secrets manager or CI without ever touching disk.
const masterKeyEnvVar = "RESTORELAB_MASTER_KEY"

// defaultKeyRelPath is where LoadKey looks as a last resort, relative to the
// user's home directory.
const defaultKeyRelPath = ".restorelab/master.key"

// defaultKeyPath returns ~/.restorelab/master.key for the current user.
func defaultKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(defaultKeyRelPath)), nil
}

// LoadKey resolves the RestoreLab master key, trying in order:
//
//  1. the RESTORELAB_MASTER_KEY environment variable (base64 or hex),
//  2. the file at explicitPath, when explicitPath is non-empty,
//  3. ~/.restorelab/master.key.
//
// It returns the key together with a short human-readable description of
// which source it came from, so callers can tell the user where their key is
// coming from. When none of the three sources exist, it returns ErrNoKey so
// the caller can offer to create one instead of failing outright.
func LoadKey(explicitPath string) (Key, string, error) {
	if env, ok := os.LookupEnv(masterKeyEnvVar); ok && env != "" {
		k, err := ParseKey(env)
		if err != nil {
			return Key{}, "", fmt.Errorf("%s: %w", masterKeyEnvVar, err)
		}
		return k, fmt.Sprintf("environment variable %s", masterKeyEnvVar), nil
	}

	if explicitPath != "" {
		k, err := readKeyFile(explicitPath)
		if err != nil {
			if os.IsNotExist(err) {
				return Key{}, "", ErrNoKey
			}
			return Key{}, "", fmt.Errorf("read master key from %s: %w", explicitPath, err)
		}
		return k, fmt.Sprintf("key file %s", explicitPath), nil
	}

	path, err := defaultKeyPath()
	if err != nil {
		return Key{}, "", err
	}
	k, err := readKeyFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Key{}, "", ErrNoKey
		}
		return Key{}, "", fmt.Errorf("read master key from %s: %w", path, err)
	}
	return k, fmt.Sprintf("key file %s", path), nil
}

func readKeyFile(path string) (Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err
	}
	return ParseKey(string(trimTrailingNewline(data)))
}

// trimTrailingNewline strips a trailing \n or \r\n so keys saved with a text
// editor still parse.
func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// SaveKey writes k, base64-encoded, to path with owner-only permissions
// (0600), creating parent directories (0700) as needed. It refuses to
// overwrite an existing file: overwriting a master key would silently orphan
// every secret ever sealed under the previous key, with no way to recover
// them, so callers must explicitly remove the old file first if that is
// really what they want.
//
// On Windows, os.FileMode permission bits are advisory only: the Go runtime
// maps them onto a coarse read-only attribute and does not enforce POSIX-style
// owner/group/other permissions. Treat the 0600/0700 modes here as
// documentation of intent, not as an access control guarantee on Windows.
func SaveKey(path string, k Key) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create key directory %s: %w", dir, err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite existing master key at %s (remove it first if you really mean to replace it - doing so orphans every secret sealed under the old key)", path)
		}
		return fmt.Errorf("create master key file %s: %w", path, err)
	}
	// Safety net for the error path below; the success path closes explicitly.
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(Encode(k)); err != nil {
		return fmt.Errorf("write master key file %s: %w", path, err)
	}
	// The close is checked, not deferred away. This is the master key: if the
	// final flush fails (full disk, I/O error) the file on disk is short or
	// empty, and returning nil here would tell the user their key is stored
	// when every secret sealed under it is already unrecoverable.
	if err := f.Close(); err != nil {
		return fmt.Errorf("write master key file %s: %w", path, err)
	}
	return nil
}
