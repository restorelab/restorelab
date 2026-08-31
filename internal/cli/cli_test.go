package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0 B"},
		{in: 512, want: "512 B"},
		{in: 1024, want: "1.0 KiB"},
		{in: 4 << 30, want: "4.0 GiB"},
		{in: 1536, want: "1.5 KiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 45 * time.Second, want: "45s"},
		{in: 90 * time.Second, want: "1m"},
		{in: 2*time.Hour + 4*time.Minute, want: "2h04m"},
		{in: 50 * time.Hour, want: "2d02h"},
	}
	for _, tt := range tests {
		if got := humanAge(tt.in); got != tt.want {
			t.Errorf("humanAge(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSortWorkloadsIsNumeric(t *testing.T) {
	ws := []core.Workload{{ID: "101"}, {ID: "9"}, {ID: "1002"}, {ID: "abc"}}
	sortWorkloads(ws)

	got := make([]string, len(ws))
	for i, w := range ws {
		got[i] = w.ID
	}
	want := "9 101 1002 abc"
	if strings.Join(got, " ") != want {
		t.Errorf("sorted = %v, want %q (numeric ids must not sort lexically)", got, want)
	}
}

func TestPaintIsInertWithoutColor(t *testing.T) {
	a := &app{noColor: true}
	if got := a.paint(colorRed, "boom"); got != "boom" {
		t.Errorf("paint() with colours disabled = %q, want the bare string", got)
	}
	a.noColor = false
	if got := a.paint(colorRed, "boom"); !strings.Contains(got, "boom") || !strings.HasSuffix(got, colorReset) {
		t.Errorf("paint() = %q, want a wrapped and reset string", got)
	}
}

func TestHintForKnownErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "missing config", err: fmt.Errorf("open: %w", config.ErrNotFound), want: "restorelab init"},
		{name: "missing key", err: fmt.Errorf("load: %w", crypto.ErrNoKey), want: "RESTORELAB_MASTER_KEY"},
		{name: "bad token", err: fmt.Errorf("GET /nodes: %w", core.ErrUnauthorized), want: "proxmox-permissions"},
		{name: "no backup", err: core.ErrNoBackup, want: "backup job"},
		{name: "not isolated", err: core.ErrNetworkNotIsolated, want: "network-isolation"},
		{name: "not managed", err: core.ErrNotManaged, want: "created itself"},
		{name: "unknown", err: errors.New("something else"), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hintFor(tt.err)
			if tt.want == "" {
				if got != "" {
					t.Errorf("hintFor() = %q, want no hint", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("hintFor() = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

func TestProviderEntryResolution(t *testing.T) {
	pve := config.Provider{ID: "pve", Kind: "proxmox", Roles: []string{providers.RoleHypervisor, providers.RoleBackup}}
	pve2 := config.Provider{ID: "pve2", Kind: "proxmox", Roles: []string{providers.RoleHypervisor, providers.RoleBackup}}
	pbs := config.Provider{ID: "pbs", Kind: "pbs", Roles: []string{providers.RoleBackup}}

	tests := []struct {
		name    string
		cfg     *config.Config
		id      string
		role    string
		want    string
		wantErr string
	}{
		{
			name: "explicit id",
			cfg:  &config.Config{Providers: []config.Provider{pve, pbs}},
			id:   "pbs", role: providers.RoleBackup, want: "pbs",
		},
		{
			name: "only candidate wins without configuration",
			cfg:  &config.Config{Providers: []config.Provider{pve}},
			role: providers.RoleHypervisor, want: "pve",
		},
		{
			name: "default is used",
			cfg: &config.Config{
				Providers: []config.Provider{pve, pve2},
				Defaults:  config.Defaults{Provider: "pve2"},
			},
			role: providers.RoleHypervisor, want: "pve2",
		},
		{
			name: "backup falls back to the default hypervisor",
			cfg: &config.Config{
				Providers: []config.Provider{pve, pve2},
				Defaults:  config.Defaults{Provider: "pve"},
			},
			role: providers.RoleBackup, want: "pve",
		},
		{
			name: "ambiguous without a default",
			cfg:  &config.Config{Providers: []config.Provider{pve, pve2}},
			role: providers.RoleHypervisor, wantErr: "several",
		},
		{
			name: "none configured",
			cfg:  &config.Config{},
			role: providers.RoleHypervisor, wantErr: "no hypervisor provider configured",
		},
		{
			name: "wrong role for the named provider",
			cfg:  &config.Config{Providers: []config.Provider{pbs}},
			id:   "pbs", role: providers.RoleHypervisor, wantErr: `does not have the "hypervisor" role`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app{cfg: tt.cfg}
			got, err := a.providerEntry(tt.id, tt.role)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("providerEntry() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("providerEntry() error = %v", err)
			}
			if got.ID != tt.want {
				t.Errorf("providerEntry() = %q, want %q", got.ID, tt.want)
			}
		})
	}
}

func TestReadSecretPrecedence(t *testing.T) {
	a := &app{out: &strings.Builder{}, err: &strings.Builder{}, noColor: true}

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv(tokenSecretEnv, "from-env")
		got, err := a.readSecret(&providerFlags{tokenSecret: "from-flag"})
		if err != nil || got != "from-flag" {
			t.Fatalf("readSecret() = %q, %v", got, err)
		}
	})

	t.Run("file beats env, and is trimmed", func(t *testing.T) {
		t.Setenv(tokenSecretEnv, "from-env")
		path := t.TempDir() + "/secret"
		if err := writeFile(path, "  from-file\n"); err != nil {
			t.Fatal(err)
		}
		got, err := a.readSecret(&providerFlags{secretFile: path})
		if err != nil || got != "from-file" {
			t.Fatalf("readSecret() = %q, %v", got, err)
		}
	})

	t.Run("env is the fallback", func(t *testing.T) {
		t.Setenv(tokenSecretEnv, "from-env")
		got, err := a.readSecret(&providerFlags{})
		if err != nil || got != "from-env" {
			t.Fatalf("readSecret() = %q, %v", got, err)
		}
	})

	t.Run("nothing available", func(t *testing.T) {
		t.Setenv(tokenSecretEnv, "")
		_, err := a.readSecret(&providerFlags{})
		if err == nil {
			t.Fatal("readSecret() error = nil, want a failure rather than an empty secret")
		}
	})

	t.Run("empty file is refused", func(t *testing.T) {
		t.Setenv(tokenSecretEnv, "")
		path := t.TempDir() + "/empty"
		if err := writeFile(path, "\n  \n"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.readSecret(&providerFlags{secretFile: path}); err == nil {
			t.Fatal("an empty secret file must be an error")
		}
	})
}

func TestProviderDetailsNeverLeaksSecrets(t *testing.T) {
	p := config.Provider{
		ID:          "pve",
		Kind:        "proxmox",
		TokenID:     "restorelab@pve!drills",
		TokenSecret: "rlsec:v1:supersecretsealedvalue",
		Datastore:   "main",
		Insecure:    true,
	}
	if got := providerDetails(p); strings.Contains(got, "supersecret") {
		t.Fatalf("providerDetails() leaked the token secret: %q", got)
	}
	if got := p.String(); strings.Contains(got, "supersecret") {
		t.Fatalf("Provider.String() leaked the token secret: %q", got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
