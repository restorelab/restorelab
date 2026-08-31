package crypto

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustKey(t *testing.T) Key {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := mustKey(t)
	plaintext := "super-secret-proxmox-token"

	sealed, err := Seal(k, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsSealed(sealed) {
		t.Fatalf("IsSealed(%q) = false, want true", sealed)
	}
	if !strings.HasPrefix(sealed, "rlsec:v1:") {
		t.Fatalf("sealed value missing version prefix: %q", sealed)
	}

	got, err := Open(k, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Open() = %q, want %q", got, plaintext)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	k1 := mustKey(t)
	k2 := mustKey(t)

	sealed, err := Seal(k1, "hello")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(k2, sealed); err == nil {
		t.Fatalf("Open with wrong key succeeded, want error")
	}
}

func TestOpenTamperedCiphertextFails(t *testing.T) {
	k := mustKey(t)
	sealed, err := Seal(k, "hello world")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Flip one byte in the base64 payload.
	payload := strings.TrimPrefix(sealed, sealPrefix)
	raw := []byte(payload)
	// Flip a character deep enough in the payload to hit the ciphertext,
	// not just the nonce (flipping the nonce may or may not fail depending
	// on structure, but flipping ciphertext/tag bytes always breaks GCM auth).
	idx := len(raw) - 2
	if raw[idx] == 'A' {
		raw[idx] = 'B'
	} else {
		raw[idx] = 'A'
	}
	tampered := sealPrefix + string(raw)

	if _, err := Open(k, tampered); err == nil {
		t.Fatalf("Open with tampered ciphertext succeeded, want error")
	}
}

func TestSealNonceUniqueness(t *testing.T) {
	k := mustKey(t)
	plaintext := "same-plaintext-every-time"

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		sealed, err := Seal(k, plaintext)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if seen[sealed] {
			t.Fatalf("Seal produced a duplicate output on iteration %d: %q", i, sealed)
		}
		seen[sealed] = true
	}
}

func TestOpenUnknownVersion(t *testing.T) {
	k := mustKey(t)
	_, err := Open(k, "rlsec:v99:AAAA")
	if err == nil {
		t.Fatalf("Open with unknown version succeeded, want error")
	}
	if !strings.Contains(err.Error(), "v99") {
		t.Fatalf("error %q does not mention the unknown version", err.Error())
	}
}

func TestOpenNotSealed(t *testing.T) {
	k := mustKey(t)
	if _, err := Open(k, "plaintext-not-sealed-at-all"); err == nil {
		t.Fatalf("Open on non-sealed input succeeded, want error")
	}
}

func TestParseKeyBase64(t *testing.T) {
	k := mustKey(t)
	encoded := Encode(k)

	parsed, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey(base64): %v", err)
	}
	if parsed != k {
		t.Fatalf("ParseKey(base64) roundtrip mismatch")
	}
}

func TestParseKeyHex(t *testing.T) {
	k := mustKey(t)
	hexEncoded := ""
	for _, b := range k {
		hexEncoded += hexByte(b)
	}

	parsed, err := ParseKey(hexEncoded)
	if err != nil {
		t.Fatalf("ParseKey(hex): %v", err)
	}
	if parsed != k {
		t.Fatalf("ParseKey(hex) roundtrip mismatch")
	}
}

func hexByte(b byte) string {
	const hexDigits = "0123456789abcdef"
	return string([]byte{hexDigits[b>>4], hexDigits[b&0xf]})
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"too-short",
		"not-valid-base64-or-hex!!!",
		Encode(Key{})[:10], // truncated base64, wrong length
	}
	for _, c := range cases {
		if _, err := ParseKey(c); err == nil {
			t.Errorf("ParseKey(%q) succeeded, want error", c)
		}
	}
}

func TestLoadKeyPrecedence(t *testing.T) {
	// Default path lowest precedence.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("RESTORELAB_MASTER_KEY", "")
	os.Unsetenv("RESTORELAB_MASTER_KEY")

	if _, _, err := LoadKey(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("LoadKey with nothing configured: err = %v, want ErrNoKey", err)
	}

	defaultKey := mustKey(t)
	defPath, err := defaultKeyPath()
	if err != nil {
		t.Fatalf("defaultKeyPath: %v", err)
	}
	if err := SaveKey(defPath, defaultKey); err != nil {
		t.Fatalf("SaveKey(default): %v", err)
	}

	gotKey, source, err := LoadKey("")
	if err != nil {
		t.Fatalf("LoadKey(default): %v", err)
	}
	if gotKey != defaultKey {
		t.Fatalf("LoadKey returned wrong key from default path")
	}
	if !strings.Contains(source, defPath) {
		t.Fatalf("source %q does not mention default path", source)
	}

	// Explicit path takes precedence over default.
	explicitDir := t.TempDir()
	explicitPath := filepath.Join(explicitDir, "explicit.key")
	explicitKey := mustKey(t)
	if err := SaveKey(explicitPath, explicitKey); err != nil {
		t.Fatalf("SaveKey(explicit): %v", err)
	}

	gotKey, source, err = LoadKey(explicitPath)
	if err != nil {
		t.Fatalf("LoadKey(explicit): %v", err)
	}
	if gotKey != explicitKey {
		t.Fatalf("LoadKey did not prefer explicit path over default")
	}
	if !strings.Contains(source, explicitPath) {
		t.Fatalf("source %q does not mention explicit path", source)
	}

	// Env var takes precedence over everything.
	envKey := mustKey(t)
	t.Setenv("RESTORELAB_MASTER_KEY", Encode(envKey))

	gotKey, source, err = LoadKey(explicitPath)
	if err != nil {
		t.Fatalf("LoadKey(env): %v", err)
	}
	if gotKey != envKey {
		t.Fatalf("LoadKey did not prefer env var over explicit path")
	}
	if !strings.Contains(source, "RESTORELAB_MASTER_KEY") {
		t.Fatalf("source %q does not mention env var", source)
	}
}

func TestSaveKeyRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	k1 := mustKey(t)
	k2 := mustKey(t)

	if err := SaveKey(path, k1); err != nil {
		t.Fatalf("SaveKey (first write): %v", err)
	}
	if err := SaveKey(path, k2); err == nil {
		t.Fatalf("SaveKey (second write) succeeded, want refusal to overwrite")
	}

	// Confirm the original key is untouched.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != Encode(k1) {
		t.Fatalf("SaveKey overwrote the existing key despite returning an error")
	}
}

func TestSaveKeyFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are advisory on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "master.key")
	k := mustKey(t)

	if err := SaveKey(path, k); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Fatalf("dir mode = %o, want 0700", perm)
	}
}

func TestWipe(t *testing.T) {
	k := mustKey(t)
	var zero Key
	if k == zero {
		t.Fatalf("generated key is all-zero, test is meaningless")
	}
	k.Wipe()
	if k != zero {
		t.Fatalf("Wipe did not zero the key")
	}
}
