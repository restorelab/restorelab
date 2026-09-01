package cli

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
)

// defaultListen is where serve binds unless told otherwise.
//
// Loopback, because a RestoreLab that appeared on every interface the moment
// someone typed `serve` would be a surprise, and surprises with API surfaces
// are how clusters get read by strangers. TLS is a reverse proxy's job:
// adding certificate handling to a daemon that lives behind nginx or Caddy
// would be work done twice, badly.
const defaultListen = "127.0.0.1:8080"

func newServeCmd(a *app) *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the read-only HTTP API",
		Long: `Serves RestoreLab's read-only HTTP API.

It answers questions and changes nothing: it cannot restore, start, stop or
delete anything. Triggering drills over HTTP comes later.

    restorelab serve
    restorelab serve --listen 127.0.0.1:9000

Listening anywhere but loopback needs at least one API token, created with
` + "`restorelab token create <name>`" + `. Put TLS in front of it with a
reverse proxy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serve(cmd.Context(), listen)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", defaultListen, "address to listen on")
	return cmd
}

func (a *app) serve(ctx context.Context, listen string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	history := a.store(ctx)

	// A server exposed with no authentication is not a configuration, it is
	// an accident. Refusing here is the only moment it can be prevented.
	if !isLoopbackAddr(listen) {
		tokens, err := history.ListTokens(ctx)
		if err != nil {
			return fmt.Errorf("refusing to listen on %s: the token list cannot be read: %w", listen, err)
		}
		live := 0
		for _, t := range tokens {
			if t.Live() {
				live++
			}
		}
		if live == 0 {
			return fmt.Errorf("refusing to listen on %s with no API token: create one with `restorelab token create <name>`", listen)
		}
	}

	srv := api.New(api.Options{
		History:   history,
		Tokens:    history,
		Providers: &cliProviders{a: a},
		Config:    cfg,
		Weights:   report.DefaultWeights(),
	})

	fmt.Fprintf(a.out, "%s listening on http://%s/api/v1\n", a.ok(), listen)
	fmt.Fprintf(a.out, "  history: %s\n", history.Describe())
	if isLoopbackAddr(listen) {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"loopback only; use --listen and a token to expose it, behind TLS"))
	}

	return api.Serve(ctx, listen, srv, func(format string, args ...any) {
		fmt.Fprintf(a.err, format+"\n", args...)
	})
}

// isLoopbackAddr reports whether addr binds to loopback only.
//
// Anything it cannot parse counts as public: the consequence of being wrong
// that way is being asked for a token, and the consequence of being wrong the
// other way is an unauthenticated API on a public interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// cliProviders adapts the CLI's provider wiring to what the API needs.
//
// It lives here rather than in internal/api on purpose: unsealing a provider
// secret needs the master key, and keeping that on this side of the interface
// is what stops the API package from importing crypto at all.
//
// Clients are built once and reused: proxmox.New builds an HTTP client, and
// doing that per request would throw away every connection.
type cliProviders struct {
	a *app

	mu sync.Mutex
	hv map[string]core.HypervisorProvider
	bp map[string]core.BackupProvider
}

var _ api.ProviderSet = (*cliProviders)(nil)

// Entries lists the configured providers with their secrets redacted. The
// redaction happens here so that no caller can forget it.
func (p *cliProviders) Entries() []config.Provider {
	cfg, err := p.a.config()
	if err != nil {
		return nil
	}
	out := make([]config.Provider, 0, len(cfg.Providers))
	for _, entry := range cfg.Providers {
		out = append(out, entry.Redacted())
	}
	return out
}

func (p *cliProviders) Hypervisor(id string) (core.HypervisorProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.hv[id]; ok {
		return client, nil
	}
	client, _, err := p.a.hypervisor(id)
	if err != nil {
		return nil, err
	}
	if p.hv == nil {
		p.hv = map[string]core.HypervisorProvider{}
	}
	p.hv[id] = client
	return client, nil
}

func (p *cliProviders) Backups(id string) (core.BackupProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.bp[id]; ok {
		return client, nil
	}
	client, _, err := p.a.backups(id)
	if err != nil {
		return nil, err
	}
	if p.bp == nil {
		p.bp = map[string]core.BackupProvider{}
	}
	p.bp[id] = client
	return client, nil
}
