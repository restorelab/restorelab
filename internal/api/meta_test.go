package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/diag"
)

func testConfig() *config.Config {
	cfg := config.New()
	cfg.Providers = []config.Provider{{
		ID:          "pve",
		Kind:        "proxmox",
		Roles:       []string{"hypervisor", "backup"},
		Endpoint:    "https://192.0.2.10:8006",
		TokenID:     "restorelab@pve!drills-rw",
		TokenSecret: "rlsec:v1:supersecretsealedvalue",
		Node:        "pve1",
	}}
	cfg.Defaults.Provider = "pve"
	cfg.Defaults.Network = "isolated"
	return cfg
}

func TestProvidersNeverCarryASecret(t *testing.T) {
	cfg := testConfig()
	s, _ := newTestServer(t, Options{
		Config:    cfg,
		Providers: fakeProviders{hv: testFleet(t), entries: cfg.Providers},
	})

	rec := do(s, http.MethodGet, "/api/v1/providers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{"supersecret", "rlsec:v1:", "drills-rw"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/providers leaked %q: %s", forbidden, body)
		}
	}

	var p page[providerDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].ID != "pve" {
		t.Fatalf("items = %+v, want the one configured provider", p.Items)
	}
	if !p.Items[0].Default {
		t.Error("the configured default provider is not marked as the default")
	}
}

func TestDoctorAnswersInJSON(t *testing.T) {
	cfg := testConfig()
	s, _ := newTestServer(t, Options{
		Config:    cfg,
		Providers: fakeProviders{hv: testFleet(t), bp: backupSource{}, entries: cfg.Providers},
	})

	rec := do(s, http.MethodGet, "/api/v1/doctor")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var d doctorDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("body is not a diagnostic: %v", err)
	}
	if len(d.Findings) == 0 {
		t.Fatal("the diagnostic reported nothing at all")
	}
	if d.ProviderID != "pve" {
		t.Errorf("provider_id = %q, want pve", d.ProviderID)
	}
	// A diagnostic with problems is still a 200: the findings *are* the
	// answer, and a 5xx would make a dashboard show an outage where the
	// cluster merely needs configuring.
	if rec.Code != http.StatusOK {
		t.Error("a diagnostic with findings must still be a 200")
	}
}

func TestDoctorReportsAnUnreachableClusterAsAFinding(t *testing.T) {
	broken := testFleet(t)
	broken.err = core.ErrUnauthorized
	cfg := testConfig()
	s, _ := newTestServer(t, Options{
		Config:    cfg,
		Providers: fakeProviders{hv: broken, entries: cfg.Providers},
	})

	rec := do(s, http.MethodGet, "/api/v1/doctor")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unreachable cluster is what doctor is for", rec.Code)
	}

	var d doctorDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("body is not a diagnostic: %v", err)
	}
	if d.OK || d.Problems == 0 {
		t.Fatalf("diagnostic = %+v, want problems reported", d)
	}
}

// TestDoctorReportsTheNotificationChannels guards a wire, not a behaviour.
//
// diag grew a notification section, and the only thing standing between it and
// silence over HTTP is handleDoctor copying three fields into diag.Input.
// Nothing else fails if somebody drops them: the endpoint keeps answering 200
// with a perfectly good cluster diagnostic, and the operator simply never
// learns that their alerting stopped. That is the failure mode this whole
// slice exists to remove, so it gets a test of its own.
func TestDoctorReportsTheNotificationChannels(t *testing.T) {
	cfg := testConfig()
	// Still on the configuration, and still sealed, because the point of the
	// second half of this test is that no such value reaches a response. The
	// wire the first half guards now runs through the Notifications interface
	// instead of through s.cfg, so a channel is seeded there too: an
	// implementation that stopped calling Channels() would report nothing.
	cfg.Notifications = []config.Notification{{
		ID:   "ops-discord",
		Kind: "discord",
		URL:  "rlsec:v1:whatever",
	}}
	channels := newFakeNotifications()
	channels.seed("ops-discord", "discord", "https://discord.com/api/webhooks/1/rlsec:v1:whatever")

	s, _ := newTestServer(t, Options{
		Config:              cfg,
		Providers:           fakeProviders{hv: testFleet(t), bp: backupSource{}, entries: cfg.Providers},
		Notifications:       channels,
		NotifyDispatcherOff: true,
	})

	rec := do(s, http.MethodGet, "/api/v1/doctor")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var d doctorDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("body is not a diagnostic: %v", err)
	}

	var found bool
	for _, f := range d.Findings {
		if f.Area != diag.AreaNotifications {
			continue
		}
		found = true
		// The sealed value is a stand-in for the real one, and neither may
		// ever reach a response body.
		if strings.Contains(f.Detail, "rlsec:") || strings.Contains(f.Title, "rlsec:") {
			t.Errorf("a doctor finding carries a sealed secret: %q / %q", f.Title, f.Detail)
		}
	}
	if !found {
		t.Error("doctor reported nothing about a configured channel: the diag wiring in handleDoctor is gone")
	}
}
