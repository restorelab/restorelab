package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

func TestProblemStatusesTellOurFaultFromTheirs(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown run", store.ErrNotFound, http.StatusNotFound},
		{"ambiguous prefix", store.ErrAmbiguous, http.StatusConflict},
		{"no database", store.ErrNoHistory, http.StatusServiceUnavailable},
		{"unknown workload", core.ErrNotFound, http.StatusNotFound},
		{"no backup", core.ErrNoBackup, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := problemFor(tc.err).Status; got != tc.want {
				t.Errorf("problemFor(%v).Status = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestProxmoxRefusingUsIsNotOurClientsFault(t *testing.T) {
	// The distinction the whole error mapping exists for: a 401 here would
	// send the caller hunting through its own token for hours.
	if got := problemForUpstream(core.ErrUnauthorized).Status; got != http.StatusBadGateway {
		t.Errorf("upstream ErrUnauthorized = %d, want 502", got)
	}
	if got := problemForUpstream(core.ErrTimeout).Status; got != http.StatusGatewayTimeout {
		t.Errorf("upstream ErrTimeout = %d, want 504", got)
	}
	if got := problemForUpstream(context.DeadlineExceeded).Status; got != http.StatusGatewayTimeout {
		t.Errorf("upstream deadline = %d, want 504", got)
	}
	if got := problemForUpstream(errors.New("boom")).Status; got != http.StatusBadGateway {
		t.Errorf("unknown upstream error = %d, want 502", got)
	}
}

func TestProblemIsRenderedAsRFC9457(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/recovery-runs/abcd", nil)

	writeProblem(rec, r, problemFor(store.ErrNotFound))

	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if p.Status != http.StatusNotFound || rec.Code != http.StatusNotFound {
		t.Errorf("status = %d / body says %d, want 404 in both", rec.Code, p.Status)
	}
	if !strings.HasPrefix(p.Type, problemBase) {
		t.Errorf("Type = %q, want it under %q", p.Type, problemBase)
	}
	if p.Instance != "/api/v1/recovery-runs/abcd" {
		t.Errorf("Instance = %q, want the request path", p.Instance)
	}
}

func TestScrubSecretsRemovesEverythingThatLooksLikeOne(t *testing.T) {
	cases := []string{
		"token rl_ZbCf0mQZ4pO1cQ5nQyTfM3hHqTt7yZ2aVvB1nS9kL0x was rejected",
		"dial postgres://restorelab:hunter2@db.internal:5432/history: refused",
		"proxmox: PVEAPIToken=restorelab@pve!drills-rw=6f9a2c1e-0000-0000-0000-000000000000 rejected",
		"password=hunter2 in the connection string",
	}
	for _, in := range cases {
		got := scrubSecrets(in)
		for _, forbidden := range []string{"rl_ZbCf0mQZ", "hunter2", "6f9a2c1e-0000"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("scrubSecrets(%q) = %q: it still carries %q", in, got, forbidden)
			}
		}
	}
}

func TestScrubSecretsLeavesOrdinaryMessagesAlone(t *testing.T) {
	in := "no recorded drill matches \"abcd\""
	if got := scrubSecrets(in); got != in {
		t.Errorf("scrubSecrets(%q) = %q, want it unchanged", in, got)
	}
}

func TestProblemDetailIsAlwaysScrubbed(t *testing.T) {
	err := fmt.Errorf("connect: postgres://restorelab:hunter2@db:5432/history")
	if got := problemForUpstream(err).Detail; strings.Contains(got, "hunter2") {
		t.Fatalf("Detail leaked a password: %q", got)
	}
}
