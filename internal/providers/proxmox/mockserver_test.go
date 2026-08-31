package proxmox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// recordedRequest captures one request the mock PVE server received, for
// assertions on auth headers, form bodies (e.g. "was force=1 ever sent?")
// and query parameters.
type recordedRequest struct {
	Method     string
	Path       string
	Query      url.Values
	Form       url.Values
	AuthHeader string
}

// mockRoute is a canned response for one "METHOD path" key.
type mockRoute struct {
	status int
	body   []byte
}

// mockServer is a minimal fake PVE API: routes are registered by exact
// method+path, requests are recorded for later assertions.
type mockServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handlers map[string]mockRoute
	// sequences overrides handlers for endpoints that must answer
	// differently across successive calls (e.g. task polling).
	sequences map[string][]mockRoute
	// funcs overrides both of the above for endpoints whose response
	// depends on the request (e.g. /cluster/nextid?vmid=N probing).
	funcs map[string]func(r *http.Request) mockRoute
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	m := &mockServer{
		t:         t,
		handlers:  make(map[string]mockRoute),
		sequences: make(map[string][]mockRoute),
		funcs:     make(map[string]func(r *http.Request) mockRoute),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.route))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockServer) url() string { return m.srv.URL }

// on registers a single canned JSON response. data is wrapped in the PVE
// {"data": ...} envelope automatically.
func (m *mockServer) on(method, path string, status int, data any) {
	body, err := json.Marshal(struct {
		Data any `json:"data"`
	}{Data: data})
	if err != nil {
		m.t.Fatalf("mockServer.on: marshal: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[method+" "+path] = mockRoute{status: status, body: body}
}

// onError registers a canned error response (no {"data":...} envelope
// required by real PVE error bodies, but harmless if absent).
func (m *mockServer) onError(method, path string, status int, message string) {
	body, _ := json.Marshal(struct {
		Data   any               `json:"data"`
		Errors map[string]string `json:"errors"`
	}{Data: nil, Errors: map[string]string{"error": message}})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[method+" "+path] = mockRoute{status: status, body: body}
}

// onSequence registers a list of responses returned in order across
// successive calls to the same method+path; the last one repeats once
// exhausted. Used for task-status polling.
func (m *mockServer) onSequence(method, path string, routes ...mockRoute) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequences[method+" "+path] = routes
}

// onFunc registers a response computed from the incoming request, for
// endpoints where behaviour depends on query parameters (e.g. nextid?vmid=N).
func (m *mockServer) onFunc(method, path string, fn func(r *http.Request) mockRoute) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.funcs[method+" "+path] = fn
}

func jsonRoute(status int, data any) mockRoute {
	body, _ := json.Marshal(struct {
		Data any `json:"data"`
	}{Data: data})
	return mockRoute{status: status, body: body}
}

func (m *mockServer) route(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		_ = r.ParseForm()
	}
	rec := recordedRequest{
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.Query(),
		AuthHeader: r.Header.Get("Authorization"),
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		rec.Form = r.PostForm
	}

	key := r.Method + " " + r.URL.Path

	m.mu.Lock()
	m.requests = append(m.requests, rec)

	var route mockRoute
	var ok bool
	var fn func(r *http.Request) mockRoute
	if f, exists := m.funcs[key]; exists {
		fn = f
		ok = true
	} else if seq, exists := m.sequences[key]; exists && len(seq) > 0 {
		route = seq[0]
		ok = true
		if len(seq) > 1 {
			m.sequences[key] = seq[1:]
		}
	} else {
		route, ok = m.handlers[key]
	}
	m.mu.Unlock()

	if fn != nil {
		route = fn(r)
	}

	if !ok {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"route":"no mock handler for ` + key + `"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = w.Write(route.body)
}

func (m *mockServer) recorded() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// newTestProvider builds a Provider wired to the mock server, with sane
// defaults every test can override via mutate.
func newTestProvider(t *testing.T, m *mockServer, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{
		ID:          "proxmox-test",
		Endpoint:    m.url(),
		TokenID:     "root@pam!restorelab",
		TokenSecret: "test-secret-value",
		Timeout:     5 * time.Second,
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
