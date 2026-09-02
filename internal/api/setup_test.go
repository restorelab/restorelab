package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSetup stands in for the CLI. It records what it was asked and answers
// what the test wants, so the handler's guards can be exercised without a
// cluster, a master key or a file on disk.
type fakeSetup struct {
	configured bool
	calls      int
	got        SetupRequest
	result     *SetupResult
	err        error
}

func (f *fakeSetup) Configured() bool { return f.configured }

func (f *fakeSetup) Connect(_ context.Context, req SetupRequest) (*SetupResult, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

const setupSecret = "rls_TESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTES"

func okResult() *SetupResult {
	return &SetupResult{
		Steps: []SetupStep{
			{Description: "create role RestoreLabDrill", Status: "created"},
			{Description: "create user restorelab@pve", Status: "already exists"},
		},
		ProviderID: "proxmox-main",
		Node:       "pve1",
		Bridge:     "vmbr99",
		Token:      "rl_TOKENTOKENTOKENTOKENTOKENTOKENTOKENTOKENTOK",
		TokenName:  "dashboard",
	}
}

// setupServer wires a server in setup mode, the way `serve` does when it
// finds no configuration.
func setupServer(t *testing.T, s *fakeSetup) *Server {
	t.Helper()
	return New(Options{Setup: s, SetupToken: setupSecret})
}

// postSetup sends a provisioning request with an optional setup token.
//
// The Host is loopback because httptest.NewRequest defaults to example.com,
// which the transport guard rightly refuses: an administrator password does
// not travel in clear to another machine. Setting it here is what lets these
// tests exercise everything past that guard.
func postSetup(s *Server, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body))
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

const validSetupBody = `{"endpoint":"https://192.0.2.10:8006","admin_user":"root@pam",` +
	`"admin_password":"hunter2","storages":["local-zfs"],"insecure":true}`

// The screen must render before it has a token to offer: somebody who opened
// the bare URL needs to be told what to paste.
func TestSetupStateNeedsNoToken(t *testing.T) {
	s := setupServer(t, &fakeSetup{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestSetupProvisionsAndReturnsItsSteps(t *testing.T) {
	fake := &fakeSetup{result: okResult()}
	s := setupServer(t, fake)

	rec := postSetup(s, setupSecret, validSetupBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got SetupResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a result: %v", err)
	}
	if len(got.Steps) != 2 || got.Steps[0].Status != "created" {
		t.Errorf("steps = %+v, want the two the setup produced", got.Steps)
	}
	if got.Token == "" {
		t.Error("the result carries no token: the browser has nothing to open a session with")
	}
	if fake.got.AdminPassword != "hunter2" {
		t.Errorf("the password did not reach the implementation: %q", fake.got.AdminPassword)
	}
	if len(fake.got.Storages) != 1 {
		t.Errorf("storages = %v, want the one the body carried", fake.got.Storages)
	}
}

// Without the token this route is an unauthenticated endpoint that accepts a
// root password. It is the whole reason the token exists.
func TestSetupWithoutTokenIsRefused(t *testing.T) {
	fake := &fakeSetup{result: okResult()}
	s := setupServer(t, fake)

	if rec := postSetup(s, "", validSetupBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec := postSetup(s, "rls_wrong", validSetupBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("with a wrong token, status = %d, want 401", rec.Code)
	}
	if fake.calls != 0 {
		t.Errorf("provisioning ran %d times without a valid token", fake.calls)
	}
}

// One use, whatever the outcome. A token that survived its first request
// would be a password printed on a console and valid forever.
func TestSetupTokenIsSpent(t *testing.T) {
	fake := &fakeSetup{result: okResult()}
	s := setupServer(t, fake)

	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("second call: status = %d, want 401", rec.Code)
	}
	if fake.calls != 1 {
		t.Errorf("provisioning ran %d times, want 1", fake.calls)
	}
}

// Spent even on failure. Otherwise a wrong password would leave the token
// live, and the one-time promise would hold only for the happy path.
func TestSetupTokenIsSpentEvenWhenProvisioningFails(t *testing.T) {
	fake := &fakeSetup{result: &SetupResult{}, err: errors.New("proxmox: login failed")}
	s := setupServer(t, fake)

	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusBadGateway {
		t.Fatalf("first call: status = %d, want 502", rec.Code)
	}
	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("second call: status = %d, want 401", rec.Code)
	}
}

// A body with no storage would install a RestoreLab that cannot run a drill,
// which is failing while looking like succeeding.
func TestSetupRefusesWithoutAStorage(t *testing.T) {
	fake := &fakeSetup{result: okResult()}
	s := setupServer(t, fake)

	body := `{"endpoint":"https://192.0.2.10:8006","admin_user":"root@pam","admin_password":"x"}`
	rec := postSetup(s, setupSecret, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "storage") {
		t.Errorf("the refusal does not name the missing storage:\n%s", rec.Body)
	}
	if fake.calls != 0 {
		t.Error("provisioning ran with no storage to restore onto")
	}
}

// A failure is worth more than an error string: the steps say how far it got,
// and the provisioning is idempotent so running it again is safe.
func TestSetupFailureCarriesTheStepsAlreadyDone(t *testing.T) {
	fake := &fakeSetup{
		result: &SetupResult{Steps: []SetupStep{
			{Description: "create role RestoreLabDrill", Status: "created"},
		}},
		err: errors.New("proxmox: create user: 403 Permission check failed"),
	}
	s := setupServer(t, fake)

	rec := postSetup(s, setupSecret, validSetupBody)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "create role") {
		t.Errorf("the refusal does not say how far it got:\n%s", rec.Body)
	}
}

// The password must not come back in the answer, whatever happens.
func TestSetupNeverEchoesThePassword(t *testing.T) {
	fake := &fakeSetup{
		result: &SetupResult{Steps: []SetupStep{{Description: "login", Status: "failed"}}},
		err:    errors.New("proxmox: login failed for root@pam"),
	}
	s := setupServer(t, fake)

	rec := postSetup(s, setupSecret, validSetupBody)
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the administrator password is in the response:\n%s", rec.Body)
	}
}

// A configured server does not offer to install anything. Not 403 - absent.
func TestAConfiguredServerHasNoSetupRoutes(t *testing.T) {
	s := New(Options{Setup: &fakeSetup{configured: true}, SetupToken: setupSecret})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /setup on a configured server = %d, want 404", rec.Code)
	}
	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /setup on a configured server = %d, want 404", rec.Code)
	}
}

// A server built with no Setup at all is the ordinary case, and it must
// behave like the configured one rather than panic.
func TestNoSetupConfiguredMeansNoRoutes(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// An administrator password over plain HTTP to something that is not this
// machine is the one request this server must not accept. POST /session
// already refuses on the same rule, through the same function.
func TestSetupRefusesInClearOffLoopback(t *testing.T) {
	fake := &fakeSetup{result: okResult()}
	s := setupServer(t, fake)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(validSetupBody))
	r.Host = "restorelab.example.com"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+setupSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "tls") {
		t.Errorf("the refusal does not name TLS:\n%s", rec.Body)
	}
	if fake.calls != 0 {
		t.Error("provisioning ran over plain HTTP off loopback")
	}
	// And the token survives: refusing the transport must not cost somebody
	// the one token they were printed.
	if rec := postSetup(s, setupSecret, validSetupBody); rec.Code != http.StatusOK {
		t.Errorf("after a TLS refusal, the token no longer works: %d", rec.Code)
	}
}

// The signal serve waits on. Closing it is what hands the port over.
func TestSetupDoneClosesOnSuccessOnly(t *testing.T) {
	failing := setupServer(t, &fakeSetup{result: &SetupResult{}, err: errors.New("nope")})
	postSetup(failing, setupSecret, validSetupBody)
	select {
	case <-failing.SetupDone():
		t.Fatal("SetupDone closed after a failed setup")
	default:
	}

	ok := setupServer(t, &fakeSetup{result: okResult()})
	postSetup(ok, setupSecret, validSetupBody)
	select {
	case <-ok.SetupDone():
	default:
		t.Fatal("SetupDone did not close after a successful setup")
	}
}

// A server that has nothing configured must say so, not panic and not lie.
// 503 is the honest answer: the route exists, the deployment cannot serve it
// yet, and the setup route next door is what fixes that.
func TestSetupModeRefusesTheRestWithoutPanicking(t *testing.T) {
	s := setupServer(t, &fakeSetup{})

	for _, target := range []string{
		"/api/v1/recovery-runs",
		"/api/v1/workloads",
		"/api/v1/queue",
		"/api/v1/doctor",
		"/api/v1/plans",
		"/api/v1/providers",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503: %s", target, rec.Code, rec.Body)
		}
		if !strings.Contains(strings.ToLower(rec.Body.String()), "not configured") {
			t.Errorf("GET %s does not say the server is not configured:\n%s", target, rec.Body)
		}
	}
}

// Health answers in setup mode: it is what the browser polls while the server
// is handing the port over to its configured self.
func TestHealthAnswersInSetupMode(t *testing.T) {
	s := setupServer(t, &fakeSetup{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
