// Package proxmox implements RestoreLab's core.HypervisorProvider,
// core.BackupProvider, core.CapacityReporter and core.NetworkValidator
// contracts against the Proxmox VE REST API.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Config configures a single Proxmox VE cluster/provider instance.
type Config struct {
	// ID is the user-facing identifier of this provider instance
	// ("proxmox-main"). Returned as-is by Provider.ID.
	ID string
	// Endpoint is the PVE API root, e.g. "https://pve.example.com:8006".
	Endpoint string
	// TokenID is "user@realm!tokenname".
	TokenID string
	// TokenSecret is the API token's secret UUID. Never logged.
	TokenSecret string

	InsecureSkipVerify bool
	// CACertPEM, when set, pins the CA used to verify the PVE endpoint
	// instead of the system trust store.
	CACertPEM string

	Timeout time.Duration

	// BackupStorage restricts backup discovery to a single PVE storage.
	// When empty, every storage advertising "backup" content on the node is
	// queried.
	BackupStorage string

	// TempIDMin/TempIDMax bound the range AllocateWorkloadID hands out.
	TempIDMin int
	TempIDMax int

	// NamePrefix is a cosmetic default used by callers naming temporary
	// workloads; the provider itself does not enforce it.
	NamePrefix string
}

// Provider talks to one Proxmox VE cluster. It is safe for concurrent use:
// it holds no mutable state beyond its (read-only after New) config and an
// *http.Client, which is itself concurrency-safe.
type Provider struct {
	cfg Config
	hc  *http.Client
	// pollInterval is how often WaitForTask re-checks a running PVE task.
	// Fixed at 2s in production (see New); tests in this package may lower
	// it directly to avoid real sleeps.
	pollInterval time.Duration
}

// New validates cfg, applies defaults and returns a ready-to-use Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("proxmox: Endpoint is required")
	}
	if cfg.TokenID == "" {
		return nil, errors.New("proxmox: TokenID is required")
	}
	if cfg.TokenSecret == "" {
		return nil, errors.New("proxmox: TokenSecret is required")
	}

	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")

	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.TempIDMin <= 0 {
		cfg.TempIDMin = 9000
	}
	if cfg.TempIDMax <= 0 {
		cfg.TempIDMax = 9999
	}
	if cfg.TempIDMin > cfg.TempIDMax {
		return nil, fmt.Errorf("proxmox: TempIDMin (%d) must be <= TempIDMax (%d)", cfg.TempIDMin, cfg.TempIDMax)
	}
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = "restorelab"
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CACertPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, errors.New("proxmox: CACertPEM does not contain a valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}

	return &Provider{
		cfg: cfg,
		hc: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		pollInterval: 2 * time.Second,
	}, nil
}

// ID returns the user-facing identifier configured for this provider.
func (p *Provider) ID() string { return p.cfg.ID }

// Kind identifies the provider technology.
func (p *Provider) Kind() string { return "proxmox" }

// Ping validates credentials and reachability against the unauthenticated
// version endpoint (any authenticated endpoint would do; /version is the
// cheapest).
func (p *Provider) Ping(ctx context.Context) error {
	_, err := p.get(ctx, "/version", nil)
	return err
}

// errBodyTruncateLen bounds how much of a failing response body is echoed
// back in an error message.
const errBodyTruncateLen = 512

// pveEnvelope is the `{"data": ...}` wrapper every PVE API response uses.
type pveEnvelope struct {
	Data json.RawMessage `json:"data"`
}

func (p *Provider) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	return p.request(ctx, http.MethodGet, path, params)
}

func (p *Provider) post(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	return p.request(ctx, http.MethodPost, path, form)
}

func (p *Provider) put(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	return p.request(ctx, http.MethodPut, path, form)
}

func (p *Provider) delete(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	return p.request(ctx, http.MethodDelete, path, params)
}

// request performs one PVE API call. GET/DELETE parameters travel as a query
// string; POST/PUT parameters travel as an application/x-www-form-urlencoded
// body, which is what the PVE API expects (it does not accept JSON bodies).
func (p *Provider) request(ctx context.Context, method, path string, params url.Values) (json.RawMessage, error) {
	reqURL := p.cfg.Endpoint + "/api2/json" + path

	var bodyReader io.Reader
	switch method {
	case http.MethodGet, http.MethodDelete:
		if len(params) > 0 {
			reqURL += "?" + params.Encode()
		}
	default:
		bodyReader = strings.NewReader(params.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("proxmox: build request %s %s: %w", method, path, err)
	}
	// PVE API token auth: "PVEAPIToken=<user>@<realm>!<tokenname>=<secret>".
	// Never include this header's value in an error or log line.
	req.Header.Set("Authorization", "PVEAPIToken="+p.cfg.TokenID+"="+p.cfg.TokenSecret)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// Caller-driven cancellation/deadline, not a transient transport
			// failure: propagate as-is rather than marking it retryable.
			return nil, ctx.Err()
		}
		return nil, core.Retryable(fmt.Errorf("proxmox: %s %s: %w", method, path, err))
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if readErr != nil {
		return nil, core.Retryable(fmt.Errorf("proxmox: reading response body for %s %s: %w", method, path, readErr))
	}

	if resp.StatusCode >= 400 {
		return nil, mapStatusError(resp.StatusCode, method, path, raw)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var env pveEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("proxmox: decode response for %s %s: %w", method, path, err)
	}
	return env.Data, nil
}

func mapStatusError(status int, method, path string, body []byte) error {
	snippet := body
	if len(snippet) > errBodyTruncateLen {
		snippet = snippet[:errBodyTruncateLen]
	}
	base := fmt.Errorf("proxmox: %s %s: unexpected status %d: %s", method, path, status, snippet)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: %v", core.ErrUnauthorized, base)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %v", core.ErrNotFound, base)
	case status >= 500:
		return core.Retryable(base)
	default:
		return base
	}
}
