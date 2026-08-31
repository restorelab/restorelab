package proxmox

import (
	"context"
	"errors"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

func TestAuthHeaderFormat(t *testing.T) {
	m := newMockServer(t)
	m.on("GET", "/api2/json/version", 200, map[string]any{"version": "8.1"})
	p := newTestProvider(t, m, func(c *Config) {
		c.TokenID = "root@pam!restorelab"
		c.TokenSecret = "abcd-1234-secret"
	})

	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	reqs := m.recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	want := "PVEAPIToken=root@pam!restorelab=abcd-1234-secret"
	if got := reqs[0].AuthHeader; got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		checkFunc func(t *testing.T, err error)
	}{
		{
			name:   "401 maps to ErrUnauthorized",
			status: 401,
			checkFunc: func(t *testing.T, err error) {
				if !errors.Is(err, core.ErrUnauthorized) {
					t.Errorf("expected core.ErrUnauthorized, got %v", err)
				}
			},
		},
		{
			name:   "403 maps to ErrUnauthorized",
			status: 403,
			checkFunc: func(t *testing.T, err error) {
				if !errors.Is(err, core.ErrUnauthorized) {
					t.Errorf("expected core.ErrUnauthorized, got %v", err)
				}
			},
		},
		{
			name:   "404 maps to ErrNotFound",
			status: 404,
			checkFunc: func(t *testing.T, err error) {
				if !errors.Is(err, core.ErrNotFound) {
					t.Errorf("expected core.ErrNotFound, got %v", err)
				}
			},
		},
		{
			name:   "500 is retryable",
			status: 500,
			checkFunc: func(t *testing.T, err error) {
				if !core.IsRetryable(err) {
					t.Errorf("expected retryable error, got %v", err)
				}
			},
		},
		{
			name:   "503 is retryable",
			status: 503,
			checkFunc: func(t *testing.T, err error) {
				if !core.IsRetryable(err) {
					t.Errorf("expected retryable error, got %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockServer(t)
			m.onError("GET", "/api2/json/version", tt.status, "boom")
			p := newTestProvider(t, m, nil)

			err := p.Ping(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			tt.checkFunc(t, err)
		})
	}
}

func TestErrorMessageTruncatedAndSecretFree(t *testing.T) {
	m := newMockServer(t)
	big := make([]byte, 2000)
	for i := range big {
		big[i] = 'x'
	}
	m.mu.Lock()
	m.handlers["GET /api2/json/version"] = mockRoute{status: 500, body: big}
	m.mu.Unlock()

	p := newTestProvider(t, m, func(c *Config) { c.TokenSecret = "super-secret-value" })
	err := p.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 700 {
		t.Errorf("error message not truncated: %d bytes", len(err.Error()))
	}
	if containsSubstr(err.Error(), "super-secret-value") {
		t.Errorf("error message leaks token secret: %s", err.Error())
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
