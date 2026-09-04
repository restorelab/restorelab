package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/store"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.53:9999", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"192.0.2.10:8080", false},
		{"[::]:8080", false},
		{"example.internal:8080", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestAnAddressWithNoPortIsNotAssumedLocal(t *testing.T) {
	// Anything unparseable must fall on the safe side: treating it as public
	// means asking for a token, which is the answer that cannot hurt.
	if isLoopbackAddr("this is not an address") {
		t.Fatal("an unparseable address was treated as loopback")
	}
}

// A server that queues drills nobody executes would be a trap: the caller
// gets a 201 and waits forever. Refusing to start says so at the only moment
// it can still be fixed.
func TestServeRefusesToQueueWithNobodyToExecute(t *testing.T) {
	a, out, _ := newTestApp(t)
	// A path that does not exist: if the refusal did not come first, this is
	// what would fail instead, and the assertions below would catch it.
	a.configPath = filepath.Join(t.TempDir(), "config.yaml")

	err := a.serve(context.Background(), serveOptions{listen: defaultListen, noWorker: true})
	if err == nil {
		t.Fatal("serve --no-worker started: a queue with no worker leaves every caller waiting forever")
	}
	if errors.Is(err, config.ErrNotFound) {
		t.Fatalf("the refusal must come before anything else is loaded, got %v", err)
	}

	// It has to say how to run one elsewhere, or the flag is a dead end.
	msg := err.Error()
	for _, want := range []string{"--no-listen", "--worker-elsewhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q, got:\n%s", want, msg)
		}
	}
	if out.Len() != 0 {
		t.Errorf("a refused serve should print no banner, got: %q", out)
	}
}

// The split deployment itself is legitimate: the API in a DMZ, the worker on
// the administration network, one database between them. Saying so is what
// separates it from the accident above.
func TestServeAcceptsAWorkerAnnouncedElsewhere(t *testing.T) {
	opts := serveOptions{listen: defaultListen, noWorker: true, workerElsewhere: true}
	if err := opts.check(); err != nil {
		t.Fatalf("an acknowledged split deployment was refused: %v", err)
	}
}

// Neither half is a process with any reason to exist.
func TestServeRefusesToNeitherListenNorWork(t *testing.T) {
	opts := serveOptions{listen: defaultListen, noWorker: true, noListen: true, workerElsewhere: true}
	err := opts.check()
	if err == nil {
		t.Fatal("--no-worker --no-listen started a process that does nothing at all")
	}
	if !strings.Contains(err.Error(), "--no-listen") {
		t.Errorf("the refusal should name the flags it refuses, got: %v", err)
	}
}

// A worker with no API is the normal, default case and needs no ceremony.
func TestServeWorkerOnlyNeedsNoAcknowledgement(t *testing.T) {
	opts := serveOptions{listen: defaultListen, noListen: true}
	if err := opts.check(); err != nil {
		t.Fatalf("serve --no-listen was refused: %v", err)
	}
	if err := (serveOptions{listen: defaultListen}).check(); err != nil {
		t.Fatalf("a plain serve was refused: %v", err)
	}
}

// The e2e harness of B3 shipped five routes that answered ErrNoHistory in
// every test, with nothing to signal it: a nil-safe Options makes a test
// green and wrong. This test reads the wiring itself.
func TestServeWiresSessionsAndUI(t *testing.T) {
	// store.Noop is enough: the assertion is that the field is not nil, not
	// that the store behaves. A real store would test the store, not the
	// wiring.
	opts := serveAPIOptions(store.Noop{}, &config.Config{}, &cliProviders{}, &cliNotifications{}, false)

	if opts.Sessions == nil {
		t.Error("Options.Sessions is nil: every login would answer 503")
	}
	if opts.UI == nil {
		t.Error("Options.UI is nil: serve would answer 404 on /")
	}
	if opts.Plans == nil {
		t.Error("Options.Plans is nil: the catalogue routes would answer 503")
	}
	if opts.History == nil {
		t.Error("Options.History is nil: every listing would answer 503")
	}
	if opts.Tokens == nil {
		t.Error("Options.Tokens is nil: every authenticated request would fail")
	}
	if opts.Providers == nil {
		t.Error("Options.Providers is nil: the provider routes would answer 503")
	}
	if opts.Config == nil {
		t.Error("Options.Config is nil")
	}
}

// --- tokens ------------------------------------------------------------------

func TestTokenCreateOperateGrantsOperate(t *testing.T) {
	a, out, _ := newTestApp(t)

	runCLI(t, newTokenCmd(a), "create", "dash", "--operate")

	tok := onlyToken(t, a)
	if !tok.Can(store.ScopeOperate) {
		t.Fatalf("--operate produced scopes %v, want operate among them", tok.Scopes)
	}

	// An operator must know what they just handed out: this key can destroy
	// and recreate machines, and the only moment to say so is now.
	printed := strings.ToLower(out.String())
	for _, want := range []string{"trigger", "cancel", "clean"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the creation message should say the token can %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(printed, api.TokenPrefix) {
		t.Errorf("the secret was never printed, and it cannot be printed again later:\n%s", out)
	}
}

func TestTokenCreateWithoutOperateIsReadOnly(t *testing.T) {
	a, out, _ := newTestApp(t)

	runCLI(t, newTokenCmd(a), "create", "dash")

	tok := onlyToken(t, a)
	if tok.Can(store.ScopeOperate) {
		t.Fatalf("a token created without --operate can operate: scopes %v", tok.Scopes)
	}
	if len(tok.Scopes) != 1 || tok.Scopes[0] != store.ScopeRead {
		t.Errorf("scopes = %v, want exactly [%s]", tok.Scopes, store.ScopeRead)
	}
	if !strings.Contains(strings.ToLower(out.String()), "read") {
		t.Errorf("the creation message should say the token is read only, got:\n%s", out)
	}
}

func TestTokenListShowsScopes(t *testing.T) {
	a, out, _ := newTestApp(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	for _, tok := range []store.APIToken{
		{ID: "t1", Name: "dashboard", Hash: "h1", CreatedAt: now, Scopes: []string{store.ScopeRead}},
		{ID: "t2", Name: "runner", Hash: "h2", CreatedAt: now,
			Scopes: []string{store.ScopeRead, store.ScopeOperate}},
	} {
		if err := a.store(ctx).CreateToken(ctx, tok); err != nil {
			t.Fatalf("seeding %s: %v", tok.Name, err)
		}
	}

	runCLI(t, newTokenCmd(a), "list")

	listing := out.String()
	if !strings.Contains(listing, "SCOPES") {
		t.Fatalf("the listing has no SCOPES column:\n%s", listing)
	}
	// Which token can destroy machines has to be readable off the table.
	for _, line := range strings.Split(listing, "\n") {
		if strings.HasPrefix(line, "runner") && !strings.Contains(line, store.ScopeOperate) {
			t.Errorf("the operate token does not show its scope:\n%s", line)
		}
		if strings.HasPrefix(line, "dashboard") && strings.Contains(line, store.ScopeOperate) {
			t.Errorf("a read-only token appears able to operate:\n%s", line)
		}
	}
}

// --- helpers -----------------------------------------------------------------

// newTestApp wires an app onto a real, empty SQLite history in a temporary
// directory. Real rather than a fake because scopes are stored as text and
// read back as a list: a fake would assert the round trip this exercises.
func newTestApp(t *testing.T) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	s, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("opening a temporary history: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	a := &app{out: out, err: errOut, noColor: true}
	// Claiming the once is how the store is injected: a.store then hands back
	// this one and never opens the operator's real database.
	a.storeOnce.Do(func() { a.storeValue = s })
	return a, out, errOut
}

// runCLI executes a command tree and fails the test if it returns an error.
func runCLI(t *testing.T, cmd *cobra.Command, args ...string) {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
}

// onlyToken returns the single token in the app's history.
func onlyToken(t *testing.T, a *app) store.APIToken {
	t.Helper()
	tokens, err := a.store(context.Background()).ListTokens(context.Background())
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens, want exactly one", len(tokens))
	}
	return tokens[0]
}
