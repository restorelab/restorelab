package e2e

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// loopbackBase rewrites a base URL so it names localhost rather than
// 127.0.0.1.
//
// net/http/cookiejar applies the rule a browser applies: a Secure cookie goes
// back over https, or to localhost, and nowhere else. It spells that
// exemption by name, and "127.0.0.1" is not that name - so a jar pointed at
// httptest's own URL stores the session cookie and then never sends it, and
// the test fails for a reason that has nothing to do with the server. The
// name resolves to the socket httptest is already listening on, so this
// changes the label and not the destination.
func loopbackBase(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	u.Host = net.JoinHostPort("localhost", u.Port())
	return u.String()
}

// newBrowser returns a client that keeps cookies the way a browser does,
// together with the base URL to drive it with.
func newBrowser(t *testing.T, f *apiFixture) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}, loopbackBase(t, f.server.URL)
}

// A real server, a real SQLite history and a real token: a session is opened
// and a read route is then driven by the cookie alone.
//
// This is the test that would have caught the hole in B3. A nil dependency in
// api.Options is not a compile error - it becomes store.Noop and answers 503
// - so only a full round trip says whether the wiring exists.
func TestASessionDrivesTheReadAPI(t *testing.T) {
	f := newAPIFixture(t)
	client, base := newBrowser(t, f)

	resp, err := client.Post(base+"/api/v1/session", "application/json",
		strings.NewReader(`{"token":"`+f.secret+`"}`))
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: status = %d, want 200: %s", resp.StatusCode, body)
	}

	var dto struct {
		TokenName string   `json:"token_name"`
		Scopes    []string `json:"scopes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.TokenName != "e2e" {
		t.Errorf("token_name = %q, want the token the login named", dto.TokenName)
	}
	if len(dto.Scopes) == 0 {
		t.Fatal("the login response must say what this session can do")
	}

	// No Authorization header anywhere: the cookie alone.
	runs, err := client.Get(base + "/api/v1/recovery-runs")
	if err != nil {
		t.Fatalf("GET runs: %v", err)
	}
	defer func() { _ = runs.Body.Close() }()
	if runs.StatusCode != http.StatusOK {
		t.Fatalf("GET /recovery-runs by cookie: status = %d, want 200", runs.StatusCode)
	}

	// And the run is really in there: an empty page would pass too.
	body, err := io.ReadAll(runs.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), f.runID) {
		t.Errorf("the listing does not carry the fixture's run %s: %s", f.runID, body)
	}
}

func TestTheDashboardIsServedByTheSameServer(t *testing.T) {
	f := newAPIFixture(t)

	resp, err := f.server.Client().Get(f.server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// With no front-end build this is the explanation page - and it is a 200:
	// the point of the test is that / is served at all, not that it carries
	// the application.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", ct)
	}
}

func TestTheCatalogueRoutesAreWiredInE2E(t *testing.T) {
	f := newAPIFixture(t)

	status, body := f.get(t, "/api/v1/plans")
	if status == http.StatusServiceUnavailable {
		t.Fatalf("the catalogue answered 503: api.Options.Plans is not wired in this harness (%s)", body)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /plans = %d: %s", status, body)
	}
}
