// Package pbs implements RestoreLab's core.BackupProvider contract against
// the Proxmox Backup Server REST API. It reads the backup catalogue directly
// from PBS, which knows the snapshot's real size, age and verification state
// - things PVE's own storage listing either flattens or does not report at
// all. Restoring is still PVE's job; this package only answers "what is
// there, and can it be trusted".
package pbs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// maxErrorBodyBytes bounds how much of an error response body is kept in an
// error message, so a misbehaving/huge PBS response never blows up logs.
const maxErrorBodyBytes = 512

// defaultTimeout is applied when Config.Timeout is left at its zero value.
const defaultTimeout = 30 * time.Second

// Config configures a connection to one Proxmox Backup Server datastore.
//
// A Config maps to exactly one PBS datastore. A PBS instance hosting several
// datastores that RestoreLab needs to read from is represented as several
// Providers, one per Config.
type Config struct {
	// ID is the user-facing identifier of this provider instance
	// ("pbs-main"). Stamped onto every core.Backup as ProviderID.
	ID string
	// Endpoint is the PBS API root, e.g. "https://pbs.example.com:8007".
	Endpoint string
	// TokenID identifies a PBS API token, e.g. "user@pbs!restorelab".
	TokenID string
	// TokenSecret is the token's secret value. Never logged and never
	// embedded in an error message.
	TokenSecret string
	// Datastore is the PBS datastore name backups are read from.
	Datastore string
	// PVEStorage is the name under which this same PBS datastore is attached
	// as storage on the Proxmox VE side. core.Backup.ID volids are built
	// against this name rather than Datastore, because that is what PVE's
	// restore call expects. Defaults to Datastore when left empty (the
	// common case where both sides use the same name).
	PVEStorage string

	// InsecureSkipVerify disables TLS certificate verification entirely.
	// Prefer Fingerprint instead: it still authenticates the specific
	// server. Only set this for throwaway/lab setups.
	InsecureSkipVerify bool
	// CACertPEM, when set, is used as the trust root instead of the system
	// store. Ignored when Fingerprint is set, since fingerprint pinning
	// replaces chain validation entirely.
	CACertPEM string
	// Fingerprint pins the peer leaf certificate by its SHA-256 fingerprint
	// in PBS's own format ("AA:BB:CC:...", colons optional, case
	// insensitive). PBS instances are routinely deployed with a self-signed
	// certificate and PBS itself hands out this fingerprint (pveproxy /
	// `proxmox-backup-manager cert info`) for clients to pin against, the
	// same way the official pbs-client does -- so setting this is the
	// normal, secure way to talk to such an instance, not a shortcut.
	Fingerprint string

	// SkipFailedVerification, when true, makes GetLatestBackup skip over
	// snapshots whose PBS verification job reported "failed" and fall
	// through to an older, still-verified-or-unverified one. Defaults to
	// false: GetLatestBackup returns the newest snapshot regardless of its
	// verification state, because silently substituting an older backup
	// changes the recovery point a caller asked for without them asking for
	// that trade-off. Turn this on only when the caller explicitly wants
	// "newest known-good" semantics over "newest, full stop".
	SkipFailedVerification bool

	// Timeout bounds every HTTP call made to PBS. Defaults to 30s.
	Timeout time.Duration
}

// Provider implements core.BackupProvider against one Proxmox Backup Server
// datastore.
type Provider struct {
	cfg    Config
	client *http.Client
	base   string
}

// New validates cfg and builds a Provider. It does not contact PBS: call
// Ping to verify reachability and credentials.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("pbs: Endpoint is required")
	}
	if strings.TrimSpace(cfg.TokenID) == "" {
		return nil, fmt.Errorf("pbs: TokenID is required")
	}
	if strings.TrimSpace(cfg.TokenSecret) == "" {
		return nil, fmt.Errorf("pbs: TokenSecret is required")
	}
	if strings.TrimSpace(cfg.Datastore) == "" {
		return nil, fmt.Errorf("pbs: Datastore is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if strings.TrimSpace(cfg.PVEStorage) == "" {
		cfg.PVEStorage = cfg.Datastore
	}

	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	return &Provider{
		cfg:    cfg,
		client: client,
		base:   strings.TrimRight(cfg.Endpoint, "/"),
	}, nil
}

// buildTLSConfig turns the TLS-related Config fields into a *tls.Config.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{}

	if cfg.Fingerprint != "" {
		want, err := normalizeFingerprint(cfg.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("pbs: invalid Fingerprint: %w", err)
		}
		// Go's crypto/tls has no hook to plug a custom trust decision into
		// the *normal* chain-verification path -- VerifyPeerCertificate only
		// runs at all in the way we need (with the raw certificate, before
		// Go's own chain checks reject a self-signed leaf) when
		// InsecureSkipVerify is true. So pinning against a fingerprint
		// requires setting InsecureSkipVerify=true here and then doing the
		// equivalent (arguably stronger, since it is tied to one exact
		// certificate rather than a chain of trust) check ourselves below.
		// This is NOT "no verification": every connection is still rejected
		// unless the presented leaf certificate's SHA-256 fingerprint
		// matches exactly. When Fingerprint is empty this whole branch is
		// skipped and normal X.509 verification applies (unless the caller
		// separately opted into InsecureSkipVerify below).
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("pbs: server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if got := hex.EncodeToString(sum[:]); got != want {
				return fmt.Errorf("pbs: certificate fingerprint mismatch (got %s)", got)
			}
			return nil
		}
		return tlsCfg, nil
	}

	if cfg.CACertPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, fmt.Errorf("pbs: CACertPEM does not contain a valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true
	}

	return tlsCfg, nil
}

// normalizeFingerprint validates fp and returns it as lowercase hex with no
// separators, ready to compare against a computed digest.
func normalizeFingerprint(fp string) (string, error) {
	s := strings.ToLower(strings.ReplaceAll(fp, ":", ""))
	if len(s) != sha256.Size*2 {
		return "", fmt.Errorf("expected %d hex characters (colons optional), got %d", sha256.Size*2, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("not valid hex: %w", err)
	}
	return s, nil
}

// get performs an authenticated GET against the PBS API and decodes the
// "data" field of the {"data": ...} envelope into out. out may be nil when
// only the status code matters.
func (p *Provider) get(ctx context.Context, path string, query url.Values, out any) error {
	reqURL := p.base + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("pbs: building request for %s: %w", path, err)
	}
	// PBS token auth is "PBSAPIToken=<tokenid>:<secret>" -- a COLON between
	// id and secret. Proxmox VE's own token auth uses an '=' there instead
	// ("PVEAPIToken=<tokenid>=<secret>"). Mixing the two formats up between
	// the PBS and PVE clients is a classic source of a silent, confusing 401.
	req.Header.Set("Authorization", "PBSAPIToken="+p.cfg.TokenID+":"+p.cfg.TokenSecret)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// Covers both connection failures and client-side timeouts
		// (http.Client.Timeout surfaces as a *url.Error wrapping this err).
		return core.Retryable(fmt.Errorf("pbs: request to %s failed: %w", path, err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Retryable(fmt.Errorf("pbs: reading response from %s: %w", path, err))
	}

	if resp.StatusCode != http.StatusOK {
		return mapStatusError(resp.StatusCode, path, body)
	}

	if out == nil {
		return nil
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("pbs: decoding envelope from %s: %w", path, err)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("pbs: decoding data from %s: %w", path, err)
	}
	return nil
}

// mapStatusError turns a non-200 PBS response into the appropriate sentinel
// error from core, wrapping enough context to debug without ever including
// the request's Authorization header.
func mapStatusError(status int, path string, body []byte) error {
	snippet := truncateBody(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: pbs: %s: status %d: %s", core.ErrUnauthorized, path, status, snippet)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: pbs: %s: status %d: %s", core.ErrNotFound, path, status, snippet)
	case status >= 500:
		return core.Retryable(fmt.Errorf("pbs: %s: status %d: %s", path, status, snippet))
	default:
		return fmt.Errorf("pbs: %s: unexpected status %d: %s", path, status, snippet)
	}
}

// truncateBody trims a response body to a bounded, log-safe snippet.
func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= maxErrorBodyBytes {
		return s
	}
	return s[:maxErrorBodyBytes] + "...(truncated)"
}
