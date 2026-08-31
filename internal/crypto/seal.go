package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// sealPrefix is prepended to every sealed value. The "v1" component is a
// scheme version: if the sealing scheme ever changes (different cipher,
// different KDF, ...) a new prefix ("rlsec:v2:...") lets Open dispatch on
// version and lets old and new sealed values coexist during a migration,
// instead of silently misinterpreting bytes sealed under a different scheme.
const sealPrefix = "rlsec:v1:"

// nonceSize is the standard GCM nonce length.
const nonceSize = 12

// IsSealed reports whether s looks like a value produced by Seal.
func IsSealed(s string) bool {
	return strings.HasPrefix(s, sealPrefix)
}

// Seal encrypts plaintext with AES-256-GCM under k, using a fresh random
// 12-byte nonce for every call. The output is the version-prefixed,
// base64-encoded concatenation of nonce || ciphertext || GCM tag.
func Seal(k Key, plaintext string) (string, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return "", fmt.Errorf("seal secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("seal secret: %w", err)
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal secret: generate nonce: %w", err)
	}

	// Seal appends ciphertext+tag to the first argument, so passing nonce
	// gives us nonce||ciphertext||tag in one slice.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return sealPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// errAuth is the error returned for any failure to authenticate/decrypt a
// sealed value. It is deliberately non-specific: distinguishing "wrong key"
// from "corrupted ciphertext" from "tampered ciphertext" would leak
// information useful to an attacker probing sealed values, and GCM does not
// let us tell those cases apart anyway.
var errAuth = errors.New("cannot decrypt secret: wrong master key or corrupted value")

// Open decrypts a value produced by Seal. It returns a clear, specific error
// when the version prefix is missing or unrecognised (a scheme mismatch,
// not a decryption failure), and the deliberately generic errAuth when
// authentication fails.
func Open(k Key, sealed string) (string, error) {
	if !strings.HasPrefix(sealed, "rlsec:") {
		return "", fmt.Errorf("not a sealed value: missing rlsec: prefix")
	}
	if !strings.HasPrefix(sealed, sealPrefix) {
		// Extract whatever version tag is present for a useful error message.
		rest := strings.TrimPrefix(sealed, "rlsec:")
		version := rest
		if i := strings.Index(rest, ":"); i >= 0 {
			version = rest[:i]
		}
		return "", fmt.Errorf("unsupported sealed value version %q: this build only understands v1", version)
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, sealPrefix))
	if err != nil {
		return "", errAuth
	}
	if len(raw) < nonceSize {
		return "", errAuth
	}

	block, err := aes.NewCipher(k[:])
	if err != nil {
		return "", fmt.Errorf("open secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("open secret: %w", err)
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errAuth
	}
	return string(plaintext), nil
}
