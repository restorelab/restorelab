package cli

// The CLI side of first-run provisioning.
//
// internal/api declares what it needs and nothing more; this file is where the
// master key, the configuration file and Proxmox actually live. That split is
// why a handler cannot reach a sealed secret even by accident - see
// internal/api/imports_test.go, which fails if the boundary is crossed.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/store"
)

// What the wizard provisions, when nobody is there to type flags.
//
// These are the defaults `restorelab connect` declares on its own flags, and
// they are repeated here rather than reached for through cobra: a flag's
// default lives on a command, and a browser has no command. Repeating them is
// the honest way to say the wizard makes the same choices - and a test below
// fails if the two ever disagree.
const (
	setupProviderID  = "proxmox-main"
	setupServiceUser = "restorelab@pve"
	setupRoleName    = "RestoreLabDrill"
	setupPool        = "restorelab"

	// dashboardTokenName is what the browser's own credential is called in
	// `restorelab token list`. A name rather than something generated:
	// whoever reads that list a year later should be able to tell where this
	// token came from.
	dashboardTokenName = "dashboard"

	// setupTokenPrefix names the Proxmox API token the wizard creates. The
	// suffix is what makes it unique - see setupProxmoxTokenName.
	setupTokenPrefix = "drills"
)

// setupProxmoxTokenName is the Proxmox API token this installation creates.
//
// It carries a random suffix, and that is not decoration. Proxmox reveals a
// token's secret exactly once, at creation, so an installation that finds a
// token of the same name already there cannot use it: it never saw the
// secret, and RestoreLab will not delete somebody's existing token to make
// room. The CLI escapes that with --token-name; a browser has no flags, and
// telling the person installing to "choose a different TokenName" is not an
// instruction they can act on.
//
// A fresh installation therefore always makes its own token. Two installs
// against the same cluster show up as two tokens in `pveum user token list`,
// which is the truth: they hold different secrets, and only one of them is
// the machine you are looking at.
func setupProxmoxTokenName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("naming the API token: %w", err)
	}
	return setupTokenPrefix + "-" + hex.EncodeToString(b[:]), nil
}

// cliSetup drives the same provisioning `restorelab connect` drives.
type cliSetup struct{ a *app }

var _ api.Setup = (*cliSetup)(nil)

// Configured reports whether there is a cluster to serve.
//
// A file is not enough, and assuming it was is a bug this found the hard way:
// provisioning creates the configuration and the master key *before* it logs
// in to Proxmox, because both must exist for a token to be sealed into them.
// So a wrong administrator password left behind a config with no providers -
// and the next `serve` skipped the wizard, leaving somebody locked out of the
// only screen that could have fixed their typo.
//
// The question is therefore "is there a provider", not "is there a file".
//
// It reads from disk rather than through a.config(): the app caches what it
// loads, and a wizard that has just written a configuration must not be told
// by a cache that there still is not one.
func (s *cliSetup) Configured() bool {
	cfg, err := config.Load(s.a.path())
	if err != nil {
		return false
	}
	return len(cfg.Providers) > 0
}

// Connect provisions the cluster, then mints the token the browser will use.
//
// The provisioning itself is the shared sequence in provision.go - the same
// one `connect` runs, in the same order - so the two paths cannot drift.
// What is added here is the last step the CLI does not need: an API token,
// because the browser has to be able to keep talking to this server once the
// setup token is spent.
func (s *cliSetup) Connect(ctx context.Context, req api.SetupRequest) (*api.SetupResult, error) {
	out := &api.SetupResult{}

	bridge, err := s.bridgeName()
	if err != nil {
		return out, err
	}

	proxmoxToken, err := setupProxmoxTokenName()
	if err != nil {
		return out, err
	}

	provisioned, provErr := s.a.provision(ctx, provisionOptions{
		Endpoint:      req.Endpoint,
		AdminUser:     req.AdminUser,
		AdminPassword: req.AdminPassword,
		Insecure:      req.Insecure,

		ProviderID:  setupProviderID,
		ServiceUser: setupServiceUser,
		TokenName:   proxmoxToken,
		RoleName:    setupRoleName,
		Pool:        setupPool,
		Storages:    req.Storages,

		CreateBridge: req.CreateBridge,
		ApplyBridge:  req.ApplyBridge,
		BridgeName:   bridge,
	})

	// The steps come back either way. On failure they say how far it got, and
	// every one of them is idempotent, so "fix the cause and run it again" is
	// a real instruction rather than a hope.
	if provisioned != nil {
		out.Steps = setupSteps(provisioned)
		out.ProviderID = provisioned.Entry.ID
		out.Node = provisioned.Node
		out.Bridge = provisioned.Bridge
		out.BridgeApplied = provisioned.BridgeApplied
	}
	if provErr != nil {
		return out, provErr
	}

	// The browser needs its own credential: the setup token is spent, and the
	// session cookie is minted from an API token like every other session.
	// operate and manage both, because this is the person who just installed
	// RestoreLab - a dashboard that can neither run a drill nor write a plan
	// has not finished installing anything.
	secret, err := s.mintDashboardToken(ctx)
	if err != nil {
		return out, fmt.Errorf("the cluster is connected, but no dashboard token could be created: %w", err)
	}
	out.Token = secret
	out.TokenName = dashboardTokenName

	return out, nil
}

// setupSteps converts the provider's steps into the plain shape the API
// declares, so no provider type crosses the boundary.
func setupSteps(p *provisionResult) []api.SetupStep {
	steps := make([]api.SetupStep, 0, len(p.Steps))
	for _, s := range p.Steps {
		steps = append(steps, api.SetupStep{
			Description: s.Description,
			Status:      s.Status,
			Detail:      s.Detail,
		})
	}
	return steps
}

// bridgeName is the isolated bridge the configuration's network profile names.
func (s *cliSetup) bridgeName() (string, error) {
	cfg, _, _, err := s.a.ensureInitialisedQuietly()
	if err != nil {
		return "", err
	}
	name := cfg.Defaults.Network
	if name == "" {
		name = "isolated"
	}
	profile, err := cfg.Network(name)
	if err != nil {
		return "", fmt.Errorf("no isolated network profile %q in the configuration: %w", name, err)
	}
	return profile.Bridge, nil
}

// mintDashboardToken creates the API token the browser will use.
//
// The store exists now, because provisioning wrote the configuration that
// says where it lives.
func (s *cliSetup) mintDashboardToken(ctx context.Context) (string, error) {
	history := s.a.store(ctx)
	if _, none := history.(store.Noop); none {
		return "", errors.New("no history database to keep the token in: see `restorelab db status`")
	}

	secret, record, err := api.NewToken(dashboardTokenName, time.Now())
	if err != nil {
		return "", err
	}
	// Read is always present; operate and manage because this is the person
	// who just installed RestoreLab, and a dashboard that can neither run a
	// drill nor write a plan has not finished installing anything.
	record.Scopes = []string{store.ScopeRead, store.ScopeOperate, store.ScopeManage}

	if err := history.CreateToken(ctx, record); err != nil {
		return "", err
	}
	return secret, nil
}
