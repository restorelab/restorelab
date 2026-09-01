package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// staticServer builds a server serving the given files as its dashboard.
//
// newTestServer supplies fakeHistory, fakeTokens and fakeProviders; this
// only adds the UI and a real listener, because these tests care about
// headers a recorder would show just as well but about a client's view of
// redirects and content negotiation, which it would not.
func staticServer(t *testing.T, files fstest.MapFS) *httptest.Server {
	t.Helper()
	s, _ := newTestServer(t, Options{UI: files})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func builtUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>RestoreLab</title>")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}
}

// Rule 1, and the most important one: a path under /api/ that matched no
// route must never render HTML.
func TestAnUnknownAPIPathStaysJSON(t *testing.T) {
	srv := staticServer(t, builtUI())

	resp, err := srv.Client().Get(srv.URL + "/api/v1/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json: a client that asked for JSON must not get index.html", ct)
	}
}

func TestAssetsAreServedImmutable(t *testing.T) {
	srv := staticServer(t, builtUI())

	resp, err := srv.Client().Get(srv.URL + "/assets/index-abc123.js")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable: the file name carries a hash", cc)
	}
}

func TestAnUnknownPathServesTheApplication(t *testing.T) {
	srv := staticServer(t, builtUI())

	// The client's router owns /runs/94bce70d. The server must hand it the
	// application rather than a 404 it would have to interpret.
	resp, err := srv.Client().Get(srv.URL + "/runs/94bce70d")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on index.html", cc)
	}
}

func TestTheDashboardIsServedWithoutAuthentication(t *testing.T) {
	srv := staticServer(t, builtUI())

	// It serves the login screen; requiring a session to show the form that
	// creates a session would never open.
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 with no credential", resp.StatusCode)
	}

	// And the data behind it is not.
	resp2, err := srv.Client().Get(srv.URL + "/api/v1/recovery-runs")
	if err != nil {
		t.Fatalf("GET runs: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/recovery-runs = %d, want 401", resp2.StatusCode)
	}
}

func TestTheDashboardCarriesItsSecurityHeaders(t *testing.T) {
	srv := staticServer(t, builtUI())

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src") && strings.Contains(csp, "unsafe-inline") {
		// unsafe-inline is conceded on styles only. On scripts it would undo
		// the whole reason the CSP is here: a session cookie lives in this
		// browser, and HttpOnly stops a script reading it, not using it.
		t.Errorf("the CSP allows inline script: %q", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options must be nosniff")
	}
}

func TestANonGetOutsideTheAPIIs405(t *testing.T) {
	srv := staticServer(t, builtUI())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anything", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestNoDashboardCompiledInSaysSo(t *testing.T) {
	srv := staticServer(t, fstest.MapFS{}) // an empty dist

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the person hitting / is a human in a browser", resp.StatusCode)
	}
	body := make([]byte, 2048)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "not compiled") {
		t.Errorf("the page must name the cause: %q", body[:n])
	}
}

func TestNoUIAtAllStillAnswersTheAPIProblem(t *testing.T) {
	// Options.UI nil: an API-only deployment. / is a 404 problem document,
	// as it was before the dashboard existed.
	s, _ := newTestServer(t, Options{})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
}

// Without a UI, a non-GET outside /api/ answers exactly as it did before the
// dashboard existed: the 404 problem document, not the dashboard's 405.
func TestNoUIAtAllKeepsTheOldBehaviourForEveryMethod(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, srv.URL+"/whatever", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s /whatever = %d, want 404", method, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("%s /whatever: Content-Type = %q, want problem+json", method, ct)
		}
	}
}

// A dashboard is served, but a path under /api/ is still the API's: no verb,
// no depth and no extension turns it into HTML.
func TestEveryUnmatchedAPIPathStaysAProblemDocument(t *testing.T) {
	srv := staticServer(t, builtUI())

	for _, target := range []string{
		"/api/v1/nope",
		"/api/v1/recovery-runs/abc/nope",
		"/api/v2/recovery-runs",
		"/api/",
		"/api/v1/index.html",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequest(method, srv.URL+target, nil)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, target, err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, target, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
				t.Errorf("%s %s: Content-Type = %q, want problem+json", method, target, ct)
			}
		}
	}
}

// A traversal cannot climb out of the embedded filesystem: it falls back to
// the application shell rather than reaching a file beside it.
func TestATraversalCannotEscapeTheDashboard(t *testing.T) {
	srv := staticServer(t, builtUI())

	resp, err := srv.Client().Get(srv.URL + "/assets/../../etc/passwd")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the SPA fallback)", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: this is index.html, not an asset", cc)
	}
}
