package api

import (
	"net/http"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/diag"
)

// providerDTO describes a configured provider.
//
// What it leaves out is the point: no sealed secret, and no token id either.
// A token id is half a credential, and the API has no use for it that is
// worth the risk of a dashboard logging it.
type providerDTO struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Roles     []string `json:"roles,omitempty"`
	Endpoint  string   `json:"endpoint"`
	Node      string   `json:"node,omitempty"`
	Datastore string   `json:"datastore,omitempty"`
	Insecure  bool     `json:"insecure"`
	Default   bool     `json:"default,omitempty"`
}

func (s *Server) newProviderDTO(p config.Provider) providerDTO {
	dto := providerDTO{
		ID:        p.ID,
		Kind:      p.Kind,
		Roles:     p.Roles,
		Endpoint:  p.Endpoint,
		Node:      p.Node,
		Datastore: p.Datastore,
		Insecure:  p.Insecure,
	}
	if s.cfg != nil && s.cfg.Defaults.Provider == p.ID {
		dto.Default = true
	}
	return dto
}

// handleProviders serves GET /api/v1/providers.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	out := page[providerDTO]{Items: []providerDTO{}}
	for _, p := range s.providers.Entries() {
		out.Items = append(out.Items, s.newProviderDTO(p))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// findingDTO is one line of the diagnostic.
type findingDTO struct {
	Level  string `json:"level"`
	Area   string `json:"area"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// doctorDTO is the whole diagnostic.
type doctorDTO struct {
	ProviderID string       `json:"provider_id"`
	Endpoint   string       `json:"endpoint,omitempty"`
	OK         bool         `json:"ok"`
	Problems   int          `json:"problems"`
	Findings   []findingDTO `json:"findings"`
}

// handleDoctor serves GET /api/v1/doctor.
//
// It always answers 200, findings and all. A cluster that is misconfigured is
// exactly what this endpoint is for; returning 502 would make a dashboard
// draw an outage banner over a diagnostic that worked perfectly.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerID := q.Get("provider")

	hv, err := s.providers.Hypervisor(providerID)
	if err != nil || hv == nil {
		s.writeProviderProblem(w, r, providerID, err)
		return
	}

	in := diag.Input{
		Provider:   hv,
		ProviderID: hv.ID(),
		WorkloadID: q.Get("workload"),
	}
	// A missing backup provider is a finding, not a refusal: the rest of the
	// diagnostic still has something to say.
	if bp, err := s.providers.Backups(providerID); err == nil {
		in.Backups = bp
	}
	if entry, ok := s.providerEntry(hv.ID()); ok {
		in.Endpoint = entry.Endpoint
		in.Node = entry.Node
	}
	in.NetworkName, in.Network, in.NetworkErr = s.network()

	rep := diag.Run(r.Context(), in)

	dto := doctorDTO{
		ProviderID: rep.ProviderID,
		Endpoint:   rep.Endpoint,
		OK:         rep.OK(),
		Problems:   rep.Problems(),
		Findings:   []findingDTO{},
	}
	for _, f := range rep.Findings {
		dto.Findings = append(dto.Findings, findingDTO{
			Level:  string(f.Level),
			Area:   f.Area,
			Title:  scrubSecrets(f.Title),
			Detail: scrubSecrets(f.Detail),
		})
	}
	writeJSON(w, r, http.StatusOK, dto)
}

// providerEntry finds the configuration entry behind a live provider.
func (s *Server) providerEntry(id string) (config.Provider, bool) {
	for _, p := range s.providers.Entries() {
		if p.ID == id {
			return p, true
		}
	}
	return config.Provider{}, false
}

// network resolves the default network profile the diagnostic checks.
func (s *Server) network() (string, core.NetworkConfig, error) {
	name := "isolated"
	if s.cfg != nil && s.cfg.Defaults.Network != "" {
		name = s.cfg.Defaults.Network
	}
	if s.cfg == nil {
		return name, core.NetworkConfig{}, nil
	}
	net, err := s.cfg.ResolveNetwork(name)
	return name, net, err
}
