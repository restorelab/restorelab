package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/providers"
)

// The boundary this whole design rests on: the CLI is what implements it, so
// internal/api never has to know about crypto or Proxmox.
func TestCLISatisfiesSetup(t *testing.T) {
	var _ api.Setup = (*cliSetup)(nil)
}

// setupApp builds an app pointed at an empty directory.
func setupApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	return &app{noColor: true, configPath: filepath.Join(dir, "config.yaml")}
}

// Configured decides whether the wizard exists at all, so what it means is
// load-bearing.
func TestConfiguredNeedsAProviderNotJustAFile(t *testing.T) {
	a := setupApp(t)
	s := &cliSetup{a: a}

	if s.Configured() {
		t.Fatal("Configured() is true with no configuration at all")
	}

	// This is the state a failed setup leaves behind: provisioning writes the
	// configuration and the master key before it logs in to Proxmox, because
	// both must exist for a token to be sealed into them. A wrong password
	// therefore leaves a file with no providers - and treating that as
	// configured locked somebody out of the only screen that could fix their
	// typo.
	if err := config.Save(a.path(), config.New()); err != nil {
		t.Fatalf("save an empty configuration: %v", err)
	}
	if _, err := os.Stat(a.path()); err != nil {
		t.Fatalf("the empty configuration was not written: %v", err)
	}
	if s.Configured() {
		t.Error("Configured() is true for a configuration with no providers: " +
			"a failed setup would lock the wizard away")
	}

	// One provider is what makes it configured.
	cfg := config.New()
	cfg.Upsert(config.Provider{
		ID:       "proxmox-main",
		Kind:     providers.KindProxmox,
		Roles:    []string{providers.RoleHypervisor, providers.RoleBackup},
		Endpoint: "https://192.0.2.10:8006",
		TokenID:  "restorelab@pve!drills",
	})
	if err := config.Save(a.path(), cfg); err != nil {
		t.Fatalf("save a configuration with a provider: %v", err)
	}
	if !s.Configured() {
		t.Error("Configured() is false with a provider stored")
	}
}

// The wizard makes the same choices `connect` does, except for the token
// name. They are written out in two places - a flag's default lives on a
// command, and a browser has no command - so this fails if the two ever
// disagree.
//
// --token-name is deliberately absent from this list: the wizard generates a
// unique one every time, because Proxmox reveals a token secret only at
// creation and a fresh installation can never reuse one it did not see.
func TestWizardDefaultsMatchConnectFlags(t *testing.T) {
	cmd := newConnectCmd(&app{noColor: true})

	for _, tc := range []struct {
		flag string
		want string
	}{
		{"id", setupProviderID},
		{"user", setupServiceUser},
		{"pool", setupPool},
	} {
		f := cmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("connect has no --%s flag any more: the wizard's default is now unanchored", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s defaults to %q but the wizard uses %q", tc.flag, f.DefValue, tc.want)
		}
	}
}

// Two installations against the same cluster must not collide on a token
// name, because the second one could not use the first one's secret: Proxmox
// reveals it once, at creation. This is what stops a browser install failing
// on a cluster the CLI already touched, with a message telling the person to
// pick a name the wizard offers no way to pick.
func TestProxmoxTokenNameIsUniquePerInstall(t *testing.T) {
	first, err := setupProxmoxTokenName()
	if err != nil {
		t.Fatalf("setupProxmoxTokenName: %v", err)
	}
	second, err := setupProxmoxTokenName()
	if err != nil {
		t.Fatalf("setupProxmoxTokenName: %v", err)
	}

	if first == second {
		t.Error("two installations would ask Proxmox for the same token name")
	}
	if !strings.HasPrefix(first, setupTokenPrefix+"-") {
		t.Errorf("token name %q does not say what it is for", first)
	}
	// Proxmox token ids are constrained; keep it short and boring.
	if len(first) > 24 {
		t.Errorf("token name %q is %d characters, long enough to hit a limit", first, len(first))
	}
}
