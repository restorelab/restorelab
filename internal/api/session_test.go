package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// fakeSessions is a session store in a map.
type fakeSessions struct {
	sessions map[string]store.Session
	tokens   map[string]store.APIToken // by token id
	created  int
	deleted  int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{
		sessions: map[string]store.Session{},
		tokens:   map[string]store.APIToken{},
	}
}

func (f *fakeSessions) CreateSession(_ context.Context, s store.Session, _ time.Time) error {
	f.created++
	f.sessions[s.Hash] = s
	return nil
}

func (f *fakeSessions) SessionByHash(_ context.Context, hash string, now time.Time) (*store.Session, *store.APIToken, error) {
	s, ok := f.sessions[hash]
	if !ok || !now.Before(s.ExpiresAt) {
		return nil, nil, store.ErrNotFound
	}
	tok, ok := f.tokens[s.TokenID]
	if !ok || !tok.Live() {
		return nil, nil, store.ErrNotFound
	}
	return &s, &tok, nil
}

func (f *fakeSessions) DeleteSession(_ context.Context, hash string) error {
	f.deleted++
	delete(f.sessions, hash)
	return nil
}

// sessionFixture is a server with one live token and a session store.
//
// It reuses newTestServer, testSecret, fakeHistory, fakeTokens and
// fakeProviders - the doubles this package already has. The one thing it adds
// is an httptest.NewServer around the handler: the rest of the package drives
// a *Server with a recorder, and a cookie needs a real client and a real
// Set-Cookie round trip.
type sessionFixture struct {
	server   *httptest.Server
	sessions *fakeSessions
	secret   string
	token    store.APIToken
	now      time.Time
}

func newSessionFixture(t *testing.T, scopes ...string) *sessionFixture {
	t.Helper()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sessions := newFakeSessions()

	s, tokens := newTestServer(t, Options{
		Sessions: sessions,
		Now:      func() time.Time { return now },
	})

	// newTestServer's own token, named and scoped for these tests, and known
	// to the session store so the join has something to resolve.
	tok := tokens.byHash[HashToken(testSecret)]
	tok.Name = "dashboard"
	tok.Scopes = scopes
	tokens.byHash[HashToken(testSecret)] = tok
	sessions.tokens[tok.ID] = tok

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	return &sessionFixture{server: srv, sessions: sessions, secret: testSecret, token: tok, now: now}
}

// login performs POST /session and returns the response.
func (f *sessionFixture) login(t *testing.T) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"token":"` + f.secret + `"}`)
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/session", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	return resp
}

func TestLoginSetsAHardenedCookie(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	resp := f.login(t)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatalf("no %s cookie in %v", SessionCookie, resp.Cookies())
	}
	if !got.HttpOnly {
		t.Error("the cookie must be HttpOnly: a session readable in JavaScript is one an XSS can steal")
	}
	if !got.Secure {
		t.Error("the cookie must be Secure")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", got.SameSite)
	}
	if got.Path != "/" {
		t.Errorf("Path = %q, want /", got.Path)
	}
	if got.Domain != "" {
		t.Errorf("Domain = %q, want empty: __Host- forbids it", got.Domain)
	}
	if !strings.HasPrefix(got.Value, SessionPrefix) {
		t.Errorf("the session secret must be recognisable in a log: %q", got.Value)
	}
}

func TestLoginReportsTheTokenScopesWithReadImplied(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	resp := f.login(t)
	defer func() { _ = resp.Body.Close() }()

	var dto struct {
		TokenName string    `json:"token_name"`
		Scopes    []string  `json:"scopes"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.TokenName != "dashboard" {
		t.Errorf("token_name = %q, want dashboard", dto.TokenName)
	}
	// read is implied by every token, so a UI must be told about it: it is
	// what decides whether the listing screens are reachable at all.
	if len(dto.Scopes) != 2 || dto.Scopes[0] != store.ScopeRead || dto.Scopes[1] != store.ScopeOperate {
		t.Errorf("scopes = %v, want [read operate]", dto.Scopes)
	}
	if want := f.now.Add(SessionLifetime); !dto.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", dto.ExpiresAt, want)
	}
}

func TestAnUnknownTokenGetsTheSameRejectionAsEverywhereElse(t *testing.T) {
	f := newSessionFixture(t)

	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/session",
		strings.NewReader(`{"token":"rl_definitely-not-a-token"}`))
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("a refused login must set no cookie: %v", resp.Cookies())
	}
}

func TestTheBearerHeaderWinsOverTheCookie(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	// A cookie for a session that does not exist, plus a good Bearer. The
	// header decides, so the request must succeed: a request carrying both
	// is never ambiguous.
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/recovery-runs", nil)
	req.Header.Set("Authorization", "Bearer "+f.secret)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "rls_nonsense"})

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the Authorization header must decide", resp.StatusCode)
	}
}

func TestAnExpiredSessionIsRejected(t *testing.T) {
	f := newSessionFixture(t)
	cookie := f.loginCookie(t)

	// The fixture's clock is fixed, so expire the row instead of waiting.
	sess := f.sessions.sessions[HashToken(cookie.Value)]
	sess.ExpiresAt = f.now.Add(-time.Second)
	f.sessions.sessions[sess.Hash] = sess

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/recovery-runs", nil)
	req.AddCookie(cookie)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRevokingTheTokenKillsTheSession(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)
	cookie := f.loginCookie(t)

	tok := f.token
	tok.RevokedAt = f.now
	f.sessions.tokens[tok.ID] = tok

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/recovery-runs", nil)
	req.AddCookie(cookie)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked token still carried a live session: status = %d", resp.StatusCode)
	}
}

// Invariant 6.12, proven: a session widens nothing.
func TestASessionCarriesNoScopeOfItsOwn(t *testing.T) {
	f := newSessionFixture(t) // read only
	cookie := f.loginCookie(t)

	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/recovery-runs",
		strings.NewReader(`{"workload":"110"}`))
	req.Header.Set("Origin", f.server.URL)
	req.AddCookie(cookie)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a session on a read token must not trigger a drill", resp.StatusCode)
	}
	// The Origin is the server's own, so the CSRF guard let this through: the
	// 403 has to be the scope check, or this test proves the wrong thing.
	if kind := problemKind(t, resp); kind != "insufficient-scope" {
		t.Fatalf("problem = %q, want insufficient-scope: the scopes must come from the token", kind)
	}
}

// problemKind reads the type of a problem document, without its namespace.
func problemKind(t *testing.T, resp *http.Response) string {
	t.Helper()
	var p Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode the problem document: %v", err)
	}
	return strings.TrimPrefix(p.Type, problemBase)
}

func TestACookieWriteFromAnotherOriginIsRefused(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)
	cookie := f.loginCookie(t)

	for _, origin := range []string{"", "https://evil.example"} {
		req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/recovery-runs",
			strings.NewReader(`{"workload":"110"}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.AddCookie(cookie)

		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: status = %d, want 403", origin, resp.StatusCode)
		} else if kind := problemKind(t, resp); kind != "cross-origin" {
			// The token holds operate, so a 403 here can only be the CSRF
			// guard. Naming it keeps this test from passing for the wrong
			// reason the day the scopes of the fixture change.
			t.Errorf("Origin %q: problem = %q, want cross-origin", origin, kind)
		}
		_ = resp.Body.Close()
	}
}

func TestTheOriginGuardNeverAppliesToABearerRequest(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	// No Origin, no cookie, a Bearer token: a CLI. It must not be refused
	// for a header browsers send and clients do not.
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/recovery-runs",
		strings.NewReader(`{"workload":"110"}`))
	req.Header.Set("Authorization", "Bearer "+f.secret)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("a Bearer request was refused by the CSRF guard")
	}
}

func TestLoginOverPlainHTTPIsRefusedOffLoopback(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	// httptest serves on 127.0.0.1, which is exempt. Forge the Host so the
	// request looks like one to a LAN address with no TLS in front.
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/session",
		strings.NewReader(`{"token":"`+f.secret+`"}`))
	req.Host = "restorelab.lan:8080"

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a refused login must set no cookie")
	}
}

func TestForwardedProtoSatisfiesTheTLSGuard(t *testing.T) {
	f := newSessionFixture(t, store.ScopeOperate)

	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/session",
		strings.NewReader(`{"token":"`+f.secret+`"}`))
	req.Host = "restorelab.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 behind a TLS-terminating proxy", resp.StatusCode)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	f := newSessionFixture(t)
	cookie := f.loginCookie(t)

	for i := range 2 {
		req, _ := http.NewRequest(http.MethodDelete, f.server.URL+"/api/v1/session", nil)
		req.Header.Set("Origin", f.server.URL)
		req.AddCookie(cookie)

		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatalf("DELETE /session (%d): %v", i, err)
		}
		_ = resp.Body.Close()

		// The second call arrives with a cookie the store no longer knows,
		// so it is unauthenticated - and logging out when you are already
		// logged out is not a failure a UI should have to handle.
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("DELETE /session (%d): status = %d, want 204 or 401", i, resp.StatusCode)
		}
	}
}

func TestGetSessionDescribesTheCurrentOne(t *testing.T) {
	f := newSessionFixture(t, store.ScopeManage)
	cookie := f.loginCookie(t)

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/session", nil)
	req.AddCookie(cookie)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dto.Scopes) != 2 || dto.Scopes[1] != store.ScopeManage {
		t.Errorf("scopes = %v, want [read manage]", dto.Scopes)
	}
}

func TestGetSessionOnABearerRequestSaysSo(t *testing.T) {
	f := newSessionFixture(t)

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer "+f.secret)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: this route describes a cookie session", resp.StatusCode)
	}
}

func TestSessionRoutesWithoutAStoreAnswer503(t *testing.T) {
	// Options.Sessions left nil: a deployment with no history database. The
	// credential is the good one, so the only thing that can fail is storing
	// the session - and it must say so rather than pretend the login failed.
	s, _ := newTestServer(t, Options{})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Post(srv.URL+"/api/v1/session", "application/json",
		strings.NewReader(`{"token":"`+testSecret+`"}`))
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a login that stored nothing must set no cookie")
	}
}

// loginCookie logs in and returns the session cookie.
func (f *sessionFixture) loginCookie(t *testing.T) *http.Cookie {
	t.Helper()
	resp := f.login(t)
	defer func() { _ = resp.Body.Close() }()

	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie in the login response (status %d)", resp.StatusCode)
	return nil
}
