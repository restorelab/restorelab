// Package crypto seals RestoreLab provider secrets (Proxmox/PBS API tokens)
// at rest using AES-256-GCM. The master key that protects those secrets is
// never stored in the RestoreLab config file: it lives in an environment
// variable, a dedicated key file, or the user's home directory, and callers
// are expected to keep it out of version control and backups of the config.
package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the length in bytes of a RestoreLab master key (AES-256).
const KeySize = 32

// Key is a 32-byte AES-256 master key. It is a fixed-size array (not a
// slice) so that copying a Key is an explicit, visible operation rather than
// something that can alias the caller's memory unexpectedly.
type Key [KeySize]byte

// NewKey generates a new random master key using a cryptographically secure
// random source.
func NewKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generate master key: %w", err)
	}
	return k, nil
}

// Encode renders a key as standard base64, the canonical on-disk / on-screen
// representation used by SaveKey and printed to users.
func Encode(k Key) string {
	return base64.StdEncoding.EncodeToString(k[:])
}

// ParseKey parses a master key from either standard base64 or 64-character
// hex, and rejects anything that does not decode to exactly KeySize bytes.
// Accepting both formats makes it easy to hand a key to RestoreLab from
// whatever a secrets manager happens to emit.
func ParseKey(s string) (Key, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) == KeySize {
		var k Key
		copy(k[:], raw)
		return k, nil
	}
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == KeySize {
		var k Key
		copy(k[:], raw)
		return k, nil
	}
	return Key{}, fmt.Errorf("invalid master key: expected %d bytes as base64 or hex, got a value that does not decode to that length", KeySize)
}

// ErrNoKey is returned by LoadKey when no master key can be found anywhere
// in the resolution chain, so the caller can offer to create one instead of
// failing with an opaque error.
var ErrNoKey = errors.New("no master key found")

// Wipe overwrites the key's bytes with zeroes in place.
//
// This is best-effort only: Go's garbage collector may have already copied
// the key's bytes elsewhere (stack growth, escape to heap, GC compaction),
// and the runtime gives no guarantee that Wipe reaches every copy. Treat
// this as reducing the window a key stays in memory, not as a guarantee the
// key is unrecoverable afterwards.
func (k *Key) Wipe() {
	for i := range k {
		k[i] = 0
	}
}
