package api

// First-run provisioning: the routes that turn a binary with no configuration
// into a RestoreLab connected to a cluster.
//
// This file declares what the API needs and nothing more. The master key, the
// configuration file and Proxmox live on the other side of the Setup
// interface, in internal/cli - see imports_test.go, which fails if that
// boundary is ever crossed.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

// Setup is the first-run provisioning this API can drive.
//
// The CLI implements it, for the same reason it implements ProviderSet:
// provisioning writes the configuration file and seals a token with the
// master key, and neither belongs in a package that serves HTTP. Nothing from
// internal/providers crosses this boundary - the steps arrive as the plain
// data below.
type Setup interface {
	// Configured reports whether a usable configuration already exists.
	//
	// The router asks once, when it is built: a configured server does not
	// mount the setup routes at all. That is an absence rather than an
	// authorisation check, which is a stronger thing to be.
	Configured() bool

	// Connect provisions the least-privilege service account, stores its
	// sealed token, optionally creates the isolated bridge, and mints the API
	// token the browser will use.
	//
	// req.AdminPassword lives for the duration of this call. Implementations
	// must not store it, log it, or put it in an error.
	//
	// A failure returns the steps performed so far alongside the error: the
	// provisioning is idempotent, so showing partial progress lets somebody
	// fix the cause and run it again.
	Connect(ctx context.Context, req SetupRequest) (*SetupResult, error)
}

// SetupRequest is what the wizard collected.
//
// It carries everything provisioning needs in one call, because there is only
// ever one: the setup token is spent by the first request that presents it.
type SetupRequest struct {
	Endpoint      string   `json:"endpoint"`
	AdminUser     string   `json:"admin_user"`
	AdminPassword string   `json:"admin_password"`
	Storages      []string `json:"storages"`

	// Insecure skips TLS verification. Fingerprint pins a certificate
	// instead, which is what a self-signed cluster should use.
	Insecure    bool   `json:"insecure,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	// CreateBridge asks for the isolated bridge. ApplyBridge false writes the
	// node's network configuration without reloading it, which is what
	// somebody with a maintenance window wants.
	CreateBridge bool `json:"create_bridge,omitempty"`
	ApplyBridge  bool `json:"apply_bridge,omitempty"`
}

// SetupStep is one provisioning action, in the order it was performed.
//
// It mirrors proxmox.BootstrapStep deliberately rather than aliasing it:
// aliasing would put a provider type in this package's surface, which is the
// thing the boundary exists to prevent.
type SetupStep struct {
	Description string `json:"description"`
	Status      string `json:"status"`
	Detail      string `json:"detail,omitempty"`
}

// SetupResult is what provisioning produced.
type SetupResult struct {
	Steps []SetupStep `json:"steps"`

	// ProviderID is the id the cluster was stored under.
	ProviderID string `json:"provider_id"`
	// Node is the node the bridge was created on, empty when none was.
	Node string `json:"node,omitempty"`
	// Bridge is the isolated bridge's name, empty when none was created.
	Bridge string `json:"bridge,omitempty"`
	// BridgeApplied says whether the node's network configuration was
	// reloaded, or only written for the next reboot.
	BridgeApplied bool `json:"bridge_applied,omitempty"`

	// Token is the RestoreLab API token the browser will exchange for a
	// session. Returned exactly once, here, and never readable again - the
	// store keeps only its hash.
	Token string `json:"token"`
	// TokenName names it in `restorelab token list`.
	TokenName string `json:"token_name"`
}

// setupStateDTO is what an unconfigured server says about itself.
type setupStateDTO struct {
	Required bool `json:"required"`
}

// handleSetupState says that installing is possible.
//
// It needs no token: somebody who opened the bare address has to be told what
// to paste, and this answer reveals nothing a stranger could not learn by
// seeing that the port is open. On a configured server the route does not
// exist at all, so a 200 here is itself the whole message.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, setupStateDTO{Required: true})
}

// setupFailureDTO is a refusal that says how far provisioning got.
//
// It embeds Problem rather than widening writeProblem, because exactly one
// endpoint needs to carry steps and changing the shared writer for it would
// put an empty "steps" on every problem this API has ever returned.
type setupFailureDTO struct {
	Problem
	Steps []SetupStep `json:"steps,omitempty"`
}

// writeSetupFailure renders a refusal with the steps already performed.
//
// It mirrors writeProblem - same scrubbing, same problem+json content type -
// rather than going through writeJSONStatus, which would label a problem
// document as plain JSON.
func writeSetupFailure(w http.ResponseWriter, r *http.Request, p Problem, steps []SetupStep) {
	p.Instance = r.URL.Path
	p.Detail = scrubSecrets(p.Detail)

	body, err := json.Marshal(setupFailureDTO{Problem: p, Steps: steps})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// handleSetup provisions the cluster and hands back a token.
//
// The order of the guards is the point. The transport is checked first,
// because refusing a password that has already crossed the network in clear
// is too late to matter. The token is checked next, before the body: a caller
// with no token has no business having its JSON parsed.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// The same rule POST /session applies, through the same function: a
	// browser would not send a Secure cookie back over this transport, and an
	// administrator password must not travel over it either.
	if !secureTransport(r) {
		writeBadRequest(w, r,
			"refusing to set up over plain HTTP from another machine: "+
				"put TLS in front of RestoreLab, or open this from the machine serving it")
		return
	}

	offered, _ := bearerToken(r.Header.Get("Authorization"))
	if !s.spendSetupToken(offered) {
		writeUnauthorized(w, r)
		return
	}

	var req SetupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeBadRequest(w, r, "the body is not the JSON this endpoint expects")
		return
	}
	if len(req.Storages) == 0 {
		writeBadRequest(w, r,
			"at least one storage is required: it is where a drill restores its clone, "+
				"and without one no real drill can run")
		return
	}

	result, err := s.setup.Connect(r.Context(), req)
	if err != nil {
		// The steps matter more than the message: they say how far it got,
		// and the provisioning is idempotent, so it can simply be run again.
		writeSetupFailure(w, r, newProblem("setup-failed", "The cluster could not be set up",
			http.StatusBadGateway, err.Error()), stepsOf(result))
		return
	}

	// Closed once, and only on success: this is what tells `serve` that the
	// configuration it lacked now exists.
	s.setupDone.Do(func() { close(s.setupDoneC) })
	writeJSON(w, r, result)
}

// stepsOf returns the steps of a result that may be nil.
func stepsOf(r *SetupResult) []SetupStep {
	if r == nil {
		return nil
	}
	return r.Steps
}

// spendSetupToken accepts the console token exactly once.
//
// Once whatever the outcome: a token still live after a failed attempt would
// be a password printed on a console and valid until the process ends, which
// is not what "one-time" means. The comparison is constant-time for the same
// reason the session's is.
func (s *Server) spendSetupToken(offered string) bool {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupToken == "" || offered == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(s.setupToken), []byte(offered)) != 1 {
		return false
	}
	s.setupToken = ""
	return true
}

// SetupDone is closed once first-run provisioning has succeeded.
//
// It is how `serve` learns that the configuration it lacked now exists: it
// tears this server down and opens the real one on the same port. A server
// that was never in setup mode returns a channel nothing ever closes.
func (s *Server) SetupDone() <-chan struct{} { return s.setupDoneC }
