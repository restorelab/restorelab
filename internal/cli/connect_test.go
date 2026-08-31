package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/crypto"
)

// fakeAdminPVE is the smallest Proxmox that `connect` can bootstrap against.
type fakeAdminPVE struct {
	writes   []string
	tokenID  string
	secret   string
	password string
}

func (f *fakeAdminPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")

	// Logging in is a POST but changes nothing on the cluster, so it does not
	// count as a write for the dry-run assertions.
	if r.Method != http.MethodGet && path != "/access/ticket" {
		f.writes = append(f.writes, r.Method+" "+path)
	}

	reply := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}

	switch {
	case path == "/access/ticket":
		if r.PostForm.Get("password") != f.password {
			http.Error(w, `{"errors":"authentication failure"}`, http.StatusUnauthorized)
			return
		}
		reply(map[string]any{
			"ticket":              "PVE:root@pam:DEADBEEF",
			"CSRFPreventionToken": "CSRF:1234",
			"username":            r.PostForm.Get("username"),
		})

	case path == "/version":
		reply(map[string]any{"version": "8.2.2"})

	case path == "/access/roles" && r.Method == http.MethodGet:
		reply([]map[string]any{})
	case path == "/access/users" && r.Method == http.MethodGet:
		reply([]map[string]any{})
	case path == "/pools" && r.Method == http.MethodGet:
		reply([]map[string]any{})
	case strings.HasSuffix(path, "/token") && r.Method == http.MethodGet:
		reply([]map[string]any{})

	case strings.Contains(path, "/token/") && r.Method == http.MethodPost:
		reply(map[string]any{"value": f.secret, "full-tokenid": f.tokenID})

	case path == "/nodes":
		// Reached with the freshly created token, which proves it works.
		if !strings.Contains(r.Header.Get("Authorization"), f.tokenID) {
			http.Error(w, `{"errors":"bad token"}`, http.StatusUnauthorized)
			return
		}
		reply([]map[string]any{
			{"node": "pve1", "status": "online", "maxcpu": 8, "maxmem": 1000, "mem": 100},
		})

	default:
		reply(nil)
	}
}

func TestConnectCreatesSealsAndVerifiesTheToken(t *testing.T) {
	pve := &fakeAdminPVE{
		tokenID:  "restorelab@pve!drills",
		secret:   "b6f1c0de-0000-4000-8000-000000000001",
		password: "hunter2",
	}
	srv := httptest.NewServer(pve)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	key, err := crypto.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTORELAB_MASTER_KEY", crypto.Encode(key))
	t.Setenv("RESTORELAB_CONFIG", cfgPath)

	out := &strings.Builder{}
	a := &app{out: out, err: out, noColor: true, configPath: cfgPath}

	err = a.connect(context.Background(), srv.URL, &connectFlags{
		id:            "pve-test",
		adminUser:     "root@pam",
		adminPassword: "hunter2",
		serviceUser:   "restorelab@pve",
		tokenName:     "drills",
		readOnly:      true,
		yes:           true,
	})
	if err != nil {
		t.Fatalf("connect() error = %v\n%s", err, out.String())
	}

	// The config must exist, carry the provider, and be usable afterwards.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("the config was not written: %v", err)
	}
	p, err := cfg.Provider("pve-test")
	if err != nil {
		t.Fatalf("provider not stored: %v", err)
	}
	if p.TokenID != pve.tokenID {
		t.Errorf("TokenID = %q, want %q", p.TokenID, pve.tokenID)
	}
	if cfg.Defaults.Provider != "pve-test" {
		t.Errorf("Defaults.Provider = %q, want the provider just connected", cfg.Defaults.Provider)
	}

	// The secret must be sealed on disk, and must unseal to the real value.
	if !crypto.IsSealed(p.TokenSecret) {
		t.Fatalf("token_secret is not sealed: %q", p.TokenSecret)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pve.secret) {
		t.Fatal("the token secret was written to disk in plaintext")
	}
	got, err := p.Secret(key)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if got != pve.secret {
		t.Errorf("unsealed secret = %q, want the one PVE returned", got)
	}

	// A read-only bootstrap must not create a pool.
	for _, w := range pve.writes {
		if strings.Contains(w, "/pools") {
			t.Errorf("--read-only must not create a resource pool, got write %q", w)
		}
	}

	if !strings.Contains(out.String(), "token verified") {
		t.Errorf("connect must verify the token it created:\n%s", out.String())
	}
	if strings.Contains(out.String(), pve.secret) {
		t.Error("the token secret must never be printed")
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Error("the administrator password must never be printed")
	}
}

func TestConnectDryRunChangesNothing(t *testing.T) {
	pve := &fakeAdminPVE{tokenID: "restorelab@pve!drills", secret: "s3cr3t", password: "hunter2"}
	srv := httptest.NewServer(pve)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	key, _ := crypto.NewKey()
	t.Setenv("RESTORELAB_MASTER_KEY", crypto.Encode(key))
	t.Setenv("RESTORELAB_CONFIG", cfgPath)

	out := &strings.Builder{}
	a := &app{out: out, err: out, noColor: true, configPath: cfgPath}

	if err := a.connect(context.Background(), srv.URL, &connectFlags{
		id:            "pve-test",
		adminUser:     "root@pam",
		adminPassword: "hunter2",
		serviceUser:   "restorelab@pve",
		tokenName:     "drills",
		pool:          "restorelab",
		dryRun:        true,
	}); err != nil {
		t.Fatalf("connect() error = %v\n%s", err, out.String())
	}

	if len(pve.writes) != 0 {
		t.Errorf("a dry run must issue no writes, got %v", pve.writes)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("a dry run must not store a provider, got %+v", cfg.Providers)
	}
}

func TestConnectRejectsABadPassword(t *testing.T) {
	pve := &fakeAdminPVE{tokenID: "x", secret: "y", password: "hunter2"}
	srv := httptest.NewServer(pve)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	key, _ := crypto.NewKey()
	t.Setenv("RESTORELAB_MASTER_KEY", crypto.Encode(key))
	t.Setenv("RESTORELAB_CONFIG", cfgPath)

	out := &strings.Builder{}
	a := &app{out: out, err: out, noColor: true, configPath: cfgPath}

	err := a.connect(context.Background(), srv.URL, &connectFlags{
		id:            "pve-test",
		adminUser:     "root@pam",
		adminPassword: "wrong",
		serviceUser:   "restorelab@pve",
		tokenName:     "drills",
		readOnly:      true,
		yes:           true,
	})
	if err == nil {
		t.Fatal("connect() error = nil, want an authentication failure")
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("the password must not appear in the error: %v", err)
	}
}
