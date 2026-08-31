package pbs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// newTestServer builds an httptest server that records every request it
// receives and dispatches based on a caller-supplied handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var requests []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func newTestProvider(t *testing.T, endpoint string, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{
		ID:          "pbs-main",
		Endpoint:    endpoint,
		TokenID:     "user@pbs!restorelab",
		TokenSecret: "supersecrettoken",
		Datastore:   "main",
		PVEStorage:  "pbs-main",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// --- Authorization header format -------------------------------------------------

func TestGet_AuthorizationHeaderFormat(t *testing.T) {
	var gotAuth string
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"version":"3.2"}}`))
	})

	p := newTestProvider(t, srv.URL, nil)
	if err := p.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// PBS auth is "PBSAPIToken=<id>:<secret>" -- a colon between id and
	// secret, unlike PVE's "PVEAPIToken=<id>=<secret>". Getting this wrong
	// is a classic silent-401 bug, so assert the exact string.
	want := "PBSAPIToken=user@pbs!restorelab:supersecrettoken"
	if gotAuth != want {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
	}
}

// --- Error mapping -----------------------------------------------------------------

func TestGet_ErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantSentAs error
		wantRetry  bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"errors":"invalid token"}`, wantSentAs: core.ErrUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{"errors":"forbidden"}`, wantSentAs: core.ErrUnauthorized},
		{name: "not found", status: http.StatusNotFound, body: `{"errors":"no such datastore"}`, wantSentAs: core.ErrNotFound},
		{name: "server error", status: http.StatusInternalServerError, body: `internal error`, wantRetry: true},
		{name: "bad gateway", status: http.StatusBadGateway, body: `bad gateway`, wantRetry: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			p := newTestProvider(t, srv.URL, nil)

			err := p.Ping(t.Context())
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tt.wantSentAs != nil && !errors.Is(err, tt.wantSentAs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tt.wantSentAs)
			}
			if tt.wantRetry != core.IsRetryable(err) {
				t.Fatalf("IsRetryable(%v) = %v, want %v", err, core.IsRetryable(err), tt.wantRetry)
			}
		})
	}
}

func TestGet_ErrorBodyTruncated(t *testing.T) {
	longBody := make([]byte, 5000)
	for i := range longBody {
		longBody[i] = 'x'
	}
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(longBody)
	})
	p := newTestProvider(t, srv.URL, nil)

	err := p.Ping(t.Context())
	if err == nil {
		t.Fatalf("expected error")
	}
	if len(err.Error()) > maxErrorBodyBytes+200 {
		t.Fatalf("error message not truncated, len=%d", len(err.Error()))
	}
}

func TestGet_ConnectionFailureIsRetryable(t *testing.T) {
	// A server that immediately closes without responding.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("ResponseWriter does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, nil)
	err := p.Ping(t.Context())
	if err == nil {
		t.Fatalf("expected error")
	}
	if !core.IsRetryable(err) {
		t.Fatalf("expected connection failure to be retryable, got %v", err)
	}
}

// --- Snapshot -> core.Backup mapping ------------------------------------------------

func snapshotsJSON(entries string) string {
	return `{"data":[` + entries + `]}`
}

func TestListBackups_Mapping(t *testing.T) {
	// 2026-08-31T03:00:00Z
	ts := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC).Unix()

	body := snapshotsJSON(`{
		"backup-type": "vm",
		"backup-id": "101",
		"backup-time": ` + strconv.FormatInt(ts, 10) + `,
		"size": 123456789,
		"protected": true,
		"comment": "nightly",
		"verification": {"state": "ok"},
		"files": [
			{"filename": "drive-scsi0.img.fidx", "crypt-mode": "none"},
			{"filename": "index.json.blob"}
		]
	}`)

	srv, reqs := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	p := newTestProvider(t, srv.URL, nil)
	backups, err := p.ListBackups(t.Context(), "101")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("got %d backups, want 1", len(backups))
	}
	b := backups[0]

	if got, want := b.ID, "pbs-main:backup/vm/101/2026-08-31T03:00:00Z"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
	if b.WorkloadID != "101" {
		t.Fatalf("WorkloadID = %q, want 101", b.WorkloadID)
	}
	if b.ProviderID != "pbs-main" {
		t.Fatalf("ProviderID = %q, want pbs-main", b.ProviderID)
	}
	if b.Datastore != "main" {
		t.Fatalf("Datastore = %q, want main", b.Datastore)
	}
	if !b.CreatedAt.Equal(time.Unix(ts, 0).UTC()) {
		t.Fatalf("CreatedAt = %v, want %v", b.CreatedAt, time.Unix(ts, 0).UTC())
	}
	if b.SizeBytes != 123456789 {
		t.Fatalf("SizeBytes = %d, want 123456789", b.SizeBytes)
	}
	if !b.Protected {
		t.Fatalf("Protected = false, want true")
	}
	if b.Encrypted {
		t.Fatalf("Encrypted = true, want false")
	}
	if b.Verified != core.VerificationOK {
		t.Fatalf("Verified = %v, want VerificationOK", b.Verified)
	}
	if b.Notes != "nightly" {
		t.Fatalf("Notes = %q, want nightly", b.Notes)
	}

	// Check the request went to the expected path/query.
	req := (*reqs)[0]
	if req.URL.Path != "/api2/json/admin/datastore/main/snapshots" {
		t.Fatalf("path = %q", req.URL.Path)
	}
	if got := req.URL.Query().Get("backup-type"); got != "vm" {
		t.Fatalf("backup-type query = %q, want vm", got)
	}
	if got := req.URL.Query().Get("backup-id"); got != "101" {
		t.Fatalf("backup-id query = %q, want 101", got)
	}
}

func TestListBackups_VerificationStates(t *testing.T) {
	tests := []struct {
		name         string
		verification string // raw JSON for the "verification" field, or "" to omit
		want         core.VerificationState
	}{
		{name: "ok", verification: `"verification":{"state":"ok"},`, want: core.VerificationOK},
		{name: "failed", verification: `"verification":{"state":"failed"},`, want: core.VerificationFailed},
		{name: "missing", verification: ``, want: core.VerificationNone},
		{name: "unrecognised", verification: `"verification":{"state":"weird"},`, want: core.VerificationUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := snapshotsJSON(`{
				"backup-type": "vm",
				"backup-id": "101",
				"backup-time": 1000,
				"size": 1,
				` + tt.verification + `
				"files": []
			}`)
			srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			})
			p := newTestProvider(t, srv.URL, nil)
			backups, err := p.ListBackups(t.Context(), "101")
			if err != nil {
				t.Fatalf("ListBackups: %v", err)
			}
			if len(backups) != 1 {
				t.Fatalf("got %d backups, want 1", len(backups))
			}
			if backups[0].Verified != tt.want {
				t.Fatalf("Verified = %v, want %v", backups[0].Verified, tt.want)
			}
		})
	}
}

func TestListBackups_Encryption(t *testing.T) {
	tests := []struct {
		name  string
		files string
		want  bool
	}{
		{name: "none crypt mode", files: `[{"filename":"a","crypt-mode":"none"}]`, want: false},
		{name: "missing crypt mode", files: `[{"filename":"a"}]`, want: false},
		{name: "encrypted", files: `[{"filename":"a","crypt-mode":"encrypt"}]`, want: true},
		{name: "mixed", files: `[{"filename":"a","crypt-mode":"none"},{"filename":"b","crypt-mode":"encrypt"}]`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := snapshotsJSON(`{
				"backup-type": "vm",
				"backup-id": "101",
				"backup-time": 1000,
				"size": 1,
				"files": ` + tt.files + `
			}`)
			srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			})
			p := newTestProvider(t, srv.URL, nil)
			backups, err := p.ListBackups(t.Context(), "101")
			if err != nil {
				t.Fatalf("ListBackups: %v", err)
			}
			if backups[0].Encrypted != tt.want {
				t.Fatalf("Encrypted = %v, want %v", backups[0].Encrypted, tt.want)
			}
		})
	}
}

// --- volid construction --------------------------------------------------------------

func TestVolID(t *testing.T) {
	ts := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC)
	got := volID("pbs-main", "101", ts)
	want := "pbs-main:backup/vm/101/2026-08-31T03:00:00Z"
	if got != want {
		t.Fatalf("volID = %q, want %q", got, want)
	}
}

func TestListBackups_VolIDFromUnixTimestamp(t *testing.T) {
	// 2026-08-31T03:00:00Z as a unix timestamp.
	ts := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC).Unix()
	body := snapshotsJSON(`{
		"backup-type": "vm",
		"backup-id": "101",
		"backup-time": ` + strconv.FormatInt(ts, 10) + `,
		"size": 1,
		"files": []
	}`)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	p := newTestProvider(t, srv.URL, nil)
	backups, err := p.ListBackups(t.Context(), "101")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if got, want := backups[0].ID, "pbs-main:backup/vm/101/2026-08-31T03:00:00Z"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
}

// --- ordering --------------------------------------------------------------------------

func TestListBackups_NewestFirst(t *testing.T) {
	body := snapshotsJSON(
		`{"backup-type":"vm","backup-id":"101","backup-time":1000,"size":1,"files":[]},` +
			`{"backup-type":"vm","backup-id":"101","backup-time":3000,"size":1,"files":[]},` +
			`{"backup-type":"vm","backup-id":"101","backup-time":2000,"size":1,"files":[]}`,
	)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	p := newTestProvider(t, srv.URL, nil)
	backups, err := p.ListBackups(t.Context(), "101")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("got %d backups, want 3", len(backups))
	}
	for i := 0; i < len(backups)-1; i++ {
		if !backups[i].CreatedAt.After(backups[i+1].CreatedAt) {
			t.Fatalf("backups not newest-first: %v then %v", backups[i].CreatedAt, backups[i+1].CreatedAt)
		}
	}
	if !backups[0].CreatedAt.Equal(time.Unix(3000, 0).UTC()) {
		t.Fatalf("first backup CreatedAt = %v, want unix 3000", backups[0].CreatedAt)
	}
}

// --- GetLatestBackup ---------------------------------------------------------------------

func TestGetLatestBackup_EmptyDatastore(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	})
	p := newTestProvider(t, srv.URL, nil)
	_, err := p.GetLatestBackup(t.Context(), "101")
	if !errors.Is(err, core.ErrNoBackup) {
		t.Fatalf("err = %v, want core.ErrNoBackup", err)
	}
}

func TestGetLatestBackup_SkipFailedVerification(t *testing.T) {
	body := snapshotsJSON(
		`{"backup-type":"vm","backup-id":"101","backup-time":3000,"size":1,"verification":{"state":"failed"},"files":[]},` +
			`{"backup-type":"vm","backup-id":"101","backup-time":2000,"size":1,"verification":{"state":"ok"},"files":[]}`,
	)
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})

	t.Run("default returns newest regardless of verification", func(t *testing.T) {
		p := newTestProvider(t, srv.URL, nil)
		got, err := p.GetLatestBackup(t.Context(), "101")
		if err != nil {
			t.Fatalf("GetLatestBackup: %v", err)
		}
		if !got.CreatedAt.Equal(time.Unix(3000, 0).UTC()) {
			t.Fatalf("CreatedAt = %v, want unix 3000 (the failed-but-newest one)", got.CreatedAt)
		}
	})

	t.Run("SkipFailedVerification skips the failed newest snapshot", func(t *testing.T) {
		p := newTestProvider(t, srv.URL, func(c *Config) { c.SkipFailedVerification = true })
		got, err := p.GetLatestBackup(t.Context(), "101")
		if err != nil {
			t.Fatalf("GetLatestBackup: %v", err)
		}
		if !got.CreatedAt.Equal(time.Unix(2000, 0).UTC()) {
			t.Fatalf("CreatedAt = %v, want unix 2000 (the ok one)", got.CreatedAt)
		}
		if got.Verified != core.VerificationOK {
			t.Fatalf("Verified = %v, want VerificationOK", got.Verified)
		}
	})

	t.Run("SkipFailedVerification with only-failed snapshots returns ErrNoBackup", func(t *testing.T) {
		onlyFailed := snapshotsJSON(`{"backup-type":"vm","backup-id":"101","backup-time":1000,"size":1,"verification":{"state":"failed"},"files":[]}`)
		srv2, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(onlyFailed))
		})
		p := newTestProvider(t, srv2.URL, func(c *Config) { c.SkipFailedVerification = true })
		_, err := p.GetLatestBackup(t.Context(), "101")
		if !errors.Is(err, core.ErrNoBackup) {
			t.Fatalf("err = %v, want core.ErrNoBackup", err)
		}
	})
}

// --- Config validation / defaults ---------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "missing endpoint", cfg: Config{TokenID: "t", TokenSecret: "s", Datastore: "d"}, wantErr: true},
		{name: "missing token id", cfg: Config{Endpoint: "https://x:8007", TokenSecret: "s", Datastore: "d"}, wantErr: true},
		{name: "missing token secret", cfg: Config{Endpoint: "https://x:8007", TokenID: "t", Datastore: "d"}, wantErr: true},
		{name: "missing datastore", cfg: Config{Endpoint: "https://x:8007", TokenID: "t", TokenSecret: "s"}, wantErr: true},
		{name: "valid minimal", cfg: Config{Endpoint: "https://x:8007", TokenID: "t", TokenSecret: "s", Datastore: "d"}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNew_Defaults(t *testing.T) {
	p, err := New(Config{Endpoint: "https://x:8007", TokenID: "t", TokenSecret: "s", Datastore: "main"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.cfg.PVEStorage != "main" {
		t.Fatalf("PVEStorage default = %q, want main (from Datastore)", p.cfg.PVEStorage)
	}
	if p.cfg.Timeout != defaultTimeout {
		t.Fatalf("Timeout default = %v, want %v", p.cfg.Timeout, defaultTimeout)
	}
}

// --- Kind/ID -------------------------------------------------------------------------------

func TestProvider_IDAndKind(t *testing.T) {
	p := newTestProvider(t, "https://example.invalid:8007", nil)
	if p.ID() != "pbs-main" {
		t.Fatalf("ID() = %q, want pbs-main", p.ID())
	}
	if p.Kind() != "pbs" {
		t.Fatalf("Kind() = %q, want pbs", p.Kind())
	}
}
