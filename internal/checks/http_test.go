package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func newHTTPTestCheck() *httpCheck { return newHTTPCheck("http") }

func TestHTTPCheck_StatusMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{"url": srv.URL}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if res.Details["status_code"] != http.StatusOK {
		t.Fatalf("status_code = %v, want 200", res.Details["status_code"])
	}
	if _, ok := res.Details["latency_ms"]; !ok {
		t.Fatal("expected latency_ms in details")
	}
}

func TestHTTPCheck_StatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{"url": srv.URL}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
}

func TestHTTPCheck_ExpectedStatusesList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":               srv.URL,
		"expected_statuses": []any{200, 202, 204},
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}

func TestHTTPCheck_BodyContains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","database":true}`))
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":           srv.URL,
		"body_contains": "\"status\":\"ok\"",
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}

	cfg.Params["body_contains"] = "not-present"
	res = newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
}

func TestHTTPCheck_BodyMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("build 1.2.3 ready"))
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":          srv.URL,
		"body_matches": `build \d+\.\d+\.\d+ ready`,
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}

	cfg.Params["body_matches"] = `nope \d+`
	res = newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
}

func TestHTTPCheck_JSONPathEquals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":{"database":true,"queue":"ok"},"items":[{"name":"a"},{"name":"b"}]}`))
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":         srv.URL,
		"json_path":   "status.database",
		"json_equals": true,
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}

	cfg2 := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":         srv.URL,
		"json_path":   "items.1.name",
		"json_equals": "b",
	}}
	res = newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg2)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass (array index)", res.Status, res.Message)
	}

	cfg3 := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":         srv.URL,
		"json_path":   "status.database",
		"json_equals": false,
	}}
	res = newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg3)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail (value mismatch)", res.Status)
	}
}

func TestHTTPCheck_MaxBodyBytesTruncation(t *testing.T) {
	big := strings.Repeat("x", 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url":            srv.URL,
		"max_body_bytes": 100,
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if bs, ok := res.Details["body_size"].(int); !ok || bs > 100 {
		t.Fatalf("body_size = %v, want <= 100", res.Details["body_size"])
	}
	snippet, _ := res.Details["body_snippet"].(string)
	if len(snippet) > maxBodySnippet {
		t.Fatalf("body_snippet longer than %d bytes: %d", maxBodySnippet, len(snippet))
	}
}

func TestHTTPCheck_TemplateExpandedURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// srv.URL looks like http://127.0.0.1:PORT
	parts := strings.SplitN(strings.TrimPrefix(srv.URL, "http://"), ":", 2)
	host, port := parts[0], parts[1]

	cfg := core.CheckConfig{Type: "http", Timeout: 2 * time.Second, Params: map[string]any{
		"url": "http://{{ .ip }}:" + port + "/health",
	}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{IP: host}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}

func TestHTTPCheck_MissingURL(t *testing.T) {
	cfg := core.CheckConfig{Type: "http", Params: map[string]any{}}
	res := newHTTPTestCheck().Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (url is required)", res.Status)
	}
}

func TestHTTPCheck_RegisteredUnderHTTPAndHTTPS(t *testing.T) {
	r := Default()
	h, ok := r.Get("http")
	if !ok || h.Type() != "http" {
		t.Fatal("expected http to be registered")
	}
	s, ok := r.Get("https")
	if !ok || s.Type() != "https" {
		t.Fatal("expected https to be registered")
	}
}
