package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/crypto"
)

func mustKey(t *testing.T) crypto.Key {
	t.Helper()
	k, err := crypto.NewKey()
	if err != nil {
		t.Fatalf("crypto.NewKey: %v", err)
	}
	return k
}

// validConfig returns a Config that passes Validate() and Save(), with one
// sealed provider secret.
func validConfig(t *testing.T, k crypto.Key) *Config {
	t.Helper()
	c := New()
	c.Providers = []Provider{{
		ID:       "proxmox-main",
		Kind:     "proxmox",
		Roles:    []string{"hypervisor"},
		Endpoint: "https://pve.example.com:8006",
		TokenID:  "root@pam!restorelab",
		Node:     "pve1",
	}}
	if err := c.SetProviderSecret("proxmox-main", "s3cr3t-token-value", k); err != nil {
		t.Fatalf("SetProviderSecret: %v", err)
	}
	c.Defaults.Network = "isolated"
	return c
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	k := mustKey(t)

	c := validConfig(t, k)
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != c.Version {
		t.Fatalf("Version = %d, want %d", loaded.Version, c.Version)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].ID != "proxmox-main" {
		t.Fatalf("providers not round-tripped correctly: %+v", loaded.Providers)
	}
	if !crypto.IsSealed(loaded.Providers[0].TokenSecret) {
		t.Fatalf("loaded TokenSecret is not sealed: %q", loaded.Providers[0].TokenSecret)
	}
	secret, err := loaded.Providers[0].Secret(k)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret != "s3cr3t-token-value" {
		t.Fatalf("Secret() = %q, want %q", secret, "s3cr3t-token-value")
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "does-not-exist.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing file: err = %v, want ErrNotFound", err)
	}
}

func TestLoadStrictDecodingRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	bad := "version: 1\nnot_a_real_field: true\n"
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("Load with unknown field succeeded, want error")
	}
}

func TestSaveRefusesUnsealedSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	c := New()
	c.Providers = []Provider{{
		ID:          "proxmox-main",
		Kind:        "proxmox",
		Roles:       []string{"hypervisor"},
		Endpoint:    "https://pve.example.com:8006",
		TokenID:     "root@pam!restorelab",
		TokenSecret: "plaintext-oops",
	}}

	err := Save(path, c)
	if err == nil {
		t.Fatalf("Save with unsealed secret succeeded, want error")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("Save left a file behind despite refusing: %v", statErr)
	}
}

func TestProviderSecretRejectsPlaintext(t *testing.T) {
	k := mustKey(t)
	p := Provider{ID: "x", TokenSecret: "plaintext-not-sealed"}
	if _, err := p.Secret(k); err == nil {
		t.Fatalf("Secret() on plaintext TokenSecret succeeded, want error")
	}
}

func TestDefaultPathHonoursEnvVar(t *testing.T) {
	t.Setenv("RESTORELAB_CONFIG", "/custom/path/config.yaml")
	if got := DefaultPath(); got != "/custom/path/config.yaml" {
		t.Fatalf("DefaultPath() = %q, want the env override", got)
	}
}

func TestDefaultPathFallsBackToHome(t *testing.T) {
	t.Setenv("RESTORELAB_CONFIG", "")
	os.Unsetenv("RESTORELAB_CONFIG")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got := DefaultPath()
	want := filepath.Join(home, ".restorelab", "config.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestValidateDuplicateProviderIDs(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "dup", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
		{ID: "dup", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://b.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate provider id") {
		t.Fatalf("Validate() = %v, want duplicate provider id error", err)
	}
}

func TestValidateBadKind(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "vmware", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Validate() = %v, want unsupported kind error", err)
	}
}

func TestValidateBadRole(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"database"}, Endpoint: "https://a.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `role "database"`) {
		t.Fatalf("Validate() = %v, want bad role error", err)
	}
}

func TestValidateMissingNetworkProfile(t *testing.T) {
	c := New()
	c.Defaults.Network = "does-not-exist"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not name a network profile") {
		t.Fatalf("Validate() = %v, want missing network profile error", err)
	}
}

func TestValidateNonIsolatedDefaultNetwork(t *testing.T) {
	c := New()
	c.Networks["prod"] = Network{Bridge: "vmbr0", Isolated: false}
	c.Defaults.Network = "prod"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "must never be the default") {
		t.Fatalf("Validate() = %v, want non-isolated default network error", err)
	}
}

func TestValidateBadEndpoint(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "not-a-url"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("Validate() = %v, want bad endpoint error", err)
	}
}

func TestValidateTempIDRange(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com", TempIDMin: 900, TempIDMax: 800},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "temp_id_min must be less than temp_id_max") {
		t.Fatalf("Validate() = %v, want temp id range error", err)
	}
}

func TestValidateAccumulatesAllErrors(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "", Kind: "bogus", Roles: []string{"nope"}, Endpoint: "not-a-url"},
	}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate() succeeded, want multiple errors")
	}
	msg := err.Error()
	for _, want := range []string{"id is required", "not supported", `role "nope"`, "not a valid URL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Validate() error missing %q; got:\n%s", want, msg)
		}
	}
}

func TestResolveNetwork(t *testing.T) {
	c := New()
	nc, err := c.ResolveNetwork("isolated")
	if err != nil {
		t.Fatalf("ResolveNetwork: %v", err)
	}
	if nc.Bridge != "vmbr99" || !nc.Isolated {
		t.Fatalf("ResolveNetwork(isolated) = %+v, want bridge vmbr99, isolated true", nc)
	}

	if _, err := c.ResolveNetwork("missing"); err == nil {
		t.Fatalf("ResolveNetwork(missing) succeeded, want error")
	}
}

func TestProvidersByRole(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "hv", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
		{ID: "bk", Kind: "pbs", Roles: []string{"backup"}, Endpoint: "https://b.example.com"},
		{ID: "both", Kind: "proxmox", Roles: []string{"hypervisor", "backup"}, Endpoint: "https://c.example.com"},
	}
	hv := c.ProvidersByRole("hypervisor")
	if len(hv) != 2 {
		t.Fatalf("ProvidersByRole(hypervisor) returned %d providers, want 2", len(hv))
	}
	bk := c.ProvidersByRole("backup")
	if len(bk) != 2 {
		t.Fatalf("ProvidersByRole(backup) returned %d providers, want 2", len(bk))
	}
}

func TestUpsertReplacesByID(t *testing.T) {
	c := New()
	c.Upsert(Provider{ID: "x", Endpoint: "https://one.example.com"})
	c.Upsert(Provider{ID: "y", Endpoint: "https://two.example.com"})
	c.Upsert(Provider{ID: "x", Endpoint: "https://updated.example.com"})

	if len(c.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(c.Providers))
	}
	p, err := c.Provider("x")
	if err != nil {
		t.Fatalf("Provider(x): %v", err)
	}
	if p.Endpoint != "https://updated.example.com" {
		t.Fatalf("Upsert did not replace: Endpoint = %q", p.Endpoint)
	}
}

func TestRedactedAndString(t *testing.T) {
	k := mustKey(t)
	sealed, err := crypto.Seal(k, "top-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	p := Provider{ID: "x", TokenSecret: sealed}

	r := p.Redacted()
	if r.TokenSecret != "***" {
		t.Fatalf("Redacted().TokenSecret = %q, want ***", r.TokenSecret)
	}
	if strings.Contains(p.String(), sealed) {
		t.Fatalf("String() leaked the sealed secret value")
	}
	if strings.Contains(p.String(), "top-secret") {
		t.Fatalf("String() leaked the plaintext secret value")
	}
}

func TestSaveFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are advisory on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	k := mustKey(t)
	c := validConfig(t, k)

	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	k := mustKey(t)
	c := validConfig(t, k)

	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory contains unexpected entries after Save: %v", names)
	}
}
