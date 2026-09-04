package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/store"
)

// newNotifyApp wires an app onto a real config file and a real master key in
// a temporary directory.
//
// Real rather than faked, because the thing worth asserting here is what ends
// up on disk: a webhook URL is a bearer credential, and "it was sealed" is
// only true if the bytes in the file say so.
func newNotifyApp(t *testing.T) (*app, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	key, err := crypto.NewKey()
	if err != nil {
		t.Fatalf("generating a master key: %v", err)
	}
	t.Setenv("RESTORELAB_MASTER_KEY", crypto.Encode(key))
	t.Setenv(notifyURLEnv, "")

	if err := config.Save(cfgPath, config.New()); err != nil {
		t.Fatalf("writing the initial config: %v", err)
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	a := &app{out: out, err: errOut, noColor: true, configPath: cfgPath}
	return a, out, errOut, cfgPath
}

// runNotify executes the notify command tree and returns whatever it gave
// back, so a test can assert on a refusal as easily as on a success.
func runNotify(a *app, args ...string) error {
	cmd := newNotifyCmd(a)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.ExecuteContext(context.Background())
}

// A kind is the thing somebody typos, and the answer has to be the list of
// what exists rather than a trip to the documentation.
func TestNotifyAddRefusesAnUnknownKindAndNamesTheOnesThatExist(t *testing.T) {
	a, _, _, cfgPath := newNotifyApp(t)

	err := runNotify(a, "add", "ops", "--kind", "telegram", "--url", "https://example.com/hook")
	if err == nil {
		t.Fatal("notify add accepted a kind RestoreLab cannot render for")
	}
	for _, kind := range []string{"discord", "slack", "webhook"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the refusal does not name %q: %v", kind, err)
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifications) != 0 {
		t.Errorf("a refused channel was written to the config anyway: %+v", cfg.Notifications)
	}
}

// Invariant 8, at the only moment it can be enforced: the plaintext URL must
// never reach the file.
func TestNotifyAddSealsTheURLOnDisk(t *testing.T) {
	a, out, _, cfgPath := newNotifyApp(t)
	const target = "https://discord.com/api/webhooks/1/SUPERSECRETTOKEN"

	urlFile := filepath.Join(t.TempDir(), "hook.txt")
	if err := os.WriteFile(urlFile, []byte(target+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url-file", urlFile); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	if !strings.Contains(out.String(), "notify test ops") {
		t.Errorf("add does not point at the command that proves the channel works:\n%s", out.String())
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SUPERSECRETTOKEN") {
		t.Fatal("the webhook url was written to disk in plaintext")
	}
	if !strings.Contains(string(raw), "rlsec:v1:") {
		t.Fatalf("the stored url is not sealed:\n%s", raw)
	}

	// And it must unseal to exactly what was given, or the channel is
	// configured and dead.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := a.masterKey()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := cfg.Notification("ops")
	if err != nil {
		t.Fatal(err)
	}
	got, err := stored.Target(key)
	if err != nil {
		t.Fatalf("the sealed url does not open: %v", err)
	}
	if got != target {
		t.Errorf("unsealed url = %q, want the one that was given", got)
	}
}

// The warning is the point: a URL on the command line is in the shell history
// and in the process list of every user on the machine.
func TestNotifyAddWarnsAboutTheShellHistory(t *testing.T) {
	a, _, errOut, _ := newNotifyApp(t)

	if err := runNotify(a, "add", "ops", "--kind", "slack",
		"--url", "https://hooks.slack.com/services/T/B/XXXX"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	if !strings.Contains(errOut.String(), "shell history") {
		t.Errorf("no warning about the shell history:\n%s", errOut.String())
	}
}

// The https rule lives in config.SetNotificationURL, and this asserts the CLI
// goes through that door rather than around it.
func TestNotifyAddRefusesPlainHTTPToTheOutsideWorld(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)

	err := runNotify(a, "add", "ops", "--kind", "webhook", "--url", "http://example.com/hook")
	if err == nil {
		t.Fatal("a webhook url over plain http was accepted; it is a bearer credential in a request line")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// The path of a Discord webhook is the credential. A listing that showed it
// would leak it into every terminal recording and screenshot.
func TestNotifyListShowsTheHostAndNeverThePath(t *testing.T) {
	a, out, _, _ := newNotifyApp(t)

	if err := runNotify(a, "add", "ops", "--kind", "discord",
		"--url", "https://discord.com/api/webhooks/1/SUPERSECRETTOKEN"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	out.Reset()

	if err := runNotify(a, "list"); err != nil {
		t.Fatalf("notify list: %v", err)
	}
	listing := out.String()

	if !strings.Contains(listing, "discord.com") {
		t.Errorf("the listing does not show the host, so two channels cannot be told apart:\n%s", listing)
	}
	for _, secret := range []string{"SUPERSECRETTOKEN", "/api/webhooks", "rlsec:"} {
		if strings.Contains(listing, secret) {
			t.Errorf("the listing leaked %q:\n%s", secret, listing)
		}
	}
	if !strings.Contains(listing, "ops") || !strings.Contains(listing, "discord") {
		t.Errorf("the listing does not name the channel and its kind:\n%s", listing)
	}
}

func TestNotifyRemoveSaysSoWhenThereIsNothingToRemove(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)

	err := runNotify(a, "remove", "ghost")
	if err == nil {
		t.Fatal("removing a channel that does not exist reported success; somebody would believe messages had stopped")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the error does not name the id: %v", err)
	}
}

func TestNotifyRemoveTakesTheChannelOutOfTheFile(t *testing.T) {
	a, _, _, cfgPath := newNotifyApp(t)

	if err := runNotify(a, "add", "ops", "--kind", "discord",
		"--url", "https://discord.com/api/webhooks/1/tok"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	if err := runNotify(a, "remove", "ops"); err != nil {
		t.Fatalf("notify remove: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifications) != 0 {
		t.Fatalf("the channel is still configured: %+v", cfg.Notifications)
	}
}

// `notify test` is the command that makes an alerting path trustworthy, so it
// has to actually post, and it has to say what came back.
func TestNotifyTestPostsASampleAndReportsTheStatus(t *testing.T) {
	var (
		gotPath string
		gotBody []byte
		gotType string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a, out, _, _ := newNotifyApp(t)
	// Loopback http is the one case the https rule allows, and it is exactly
	// this: somebody pointing a channel at a receiver of their own.
	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url", srv.URL+"/hooks/SUPERSECRETTOKEN"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	out.Reset()

	if err := runNotify(a, "test", "ops"); err != nil {
		t.Fatalf("notify test: %v", err)
	}

	if gotPath != "/hooks/SUPERSECRETTOKEN" {
		t.Errorf("the message went to %q, not to the configured url", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	// It has to look like a real alert, and it has to say it is not one.
	if !strings.Contains(string(gotBody), "test message") {
		t.Errorf("the sample does not announce itself as a test, so somebody chases a workload that never broke:\n%s", gotBody)
	}
	if !strings.Contains(out.String(), "204") {
		t.Errorf("the status the far end answered is not reported:\n%s", out.String())
	}
}

func TestNotifyTestReportsARefusalWithoutLeakingTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such webhook", http.StatusNotFound)
	}))
	defer srv.Close()

	a, out, errOut, _ := newNotifyApp(t)
	target := srv.URL + "/hooks/SUPERSECRETTOKEN"
	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url", target); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	out.Reset()
	errOut.Reset()

	err := runNotify(a, "test", "ops")
	if err == nil {
		t.Fatal("notify test reported success against a channel that answered 404")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "ops") {
		t.Errorf("the error says neither what happened nor to which channel: %v", err)
	}
	// Whatever is printed here is copied into support threads.
	for _, text := range []string{err.Error(), out.String(), errOut.String()} {
		if strings.Contains(text, "SUPERSECRETTOKEN") {
			t.Errorf("the webhook credential was printed: %s", text)
		}
	}
}

func TestNotifyTestOnAnUnknownChannelSaysSo(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)

	err := runNotify(a, "test", "ghost")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("notify test on an unknown channel = %v, want an error naming it", err)
	}
}

// An operator who believes alerts are on when they are not misreads every
// silence that follows, so the switch that turns them off has to exist and
// has to say what it does.
func TestServeHasANoNotifyFlag(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)

	flag := newServeCmd(a).Flags().Lookup("no-notify")
	if flag == nil {
		t.Fatal("serve has no --no-notify flag")
	}
	if !strings.Contains(flag.Usage, "notification") {
		t.Errorf("--no-notify does not say what it stops: %q", flag.Usage)
	}
}

// A dispatcher must start in a process that serves the API and nothing else.
// In a split deployment the drills run elsewhere, so tying alerting to the
// worker would leave the half people look at unable to speak.
func TestTheDispatcherDoesNotDependOnTheWorker(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	a, out, _, _ := newNotifyApp(t)
	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url", srv.URL+"/hooks/tok"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	out.Reset()

	cfg, err := a.config()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Declared in this order so they run in the other one: cancel, then wait.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	opts := serveOptions{noWorker: true, workerElsewhere: true}
	if err := a.startDispatcher(ctx, cfg, &cliNotifications{a: a}, store.Noop{}, opts, &wg); err != nil {
		t.Fatalf("startDispatcher: %v", err)
	}
	if !strings.Contains(out.String(), "notifications: on, 1 channel") {
		t.Errorf("an API-only process did not start the dispatcher:\n%s", out.String())
	}
}

func TestNoNotifySaysTheChannelsAreStillConfigured(t *testing.T) {
	a, out, _, _ := newNotifyApp(t)
	if err := runNotify(a, "add", "ops", "--kind", "discord",
		"--url", "https://discord.com/api/webhooks/1/tok"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	out.Reset()

	cfg, err := a.config()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	if err := a.startDispatcher(context.Background(), cfg, &cliNotifications{a: a},
		store.Noop{}, serveOptions{noNotify: true}, &wg); err != nil {
		t.Fatalf("startDispatcher: %v", err)
	}
	wg.Wait()

	if !strings.Contains(out.String(), "--no-notify") {
		t.Errorf("a silenced process does not say why it is silent:\n%s", out.String())
	}
}

// TestReadingTheChannelsWhileTheyAreWrittenIsRaceFree is written for CI.
//
// It cannot fail on its own: two goroutines touching the same slice produce a
// wrong answer perhaps once in a million runs, and this test asserts nothing
// about the values it reads. It is `go test -race` that makes it mean
// something, and the race detector does not run on the machine this was
// written on. It runs in CI on every push, which is the only place this test
// reports anything. Locally it is a slow no-op, and that is worth saying out
// loud so nobody reads a green run here as proof of anything.
//
// What it exercises: the dashboard adds and removes channels through Save and
// Remove while the dispatcher reads the list through configured() and the API
// reads it through Channels(). Those all touch cfg.Notifications, which is one
// slice held by one *config.Config for the life of the process. Before this
// tranche the reader took no lock at all.
func TestReadingTheChannelsWhileTheyAreWrittenIsRaceFree(t *testing.T) {
	const target = "https://discord.com/api/webhooks/1/SUPERSECRETTOKEN"

	a, _, _, _ := newNotifyApp(t)
	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url", target); err != nil {
		t.Fatalf("notify add: %v", err)
	}

	// One adapter, as serve wires it: the mutex only serialises anything if
	// the reader and the writer are holding the same one.
	channels := &cliNotifications{a: a}

	const rounds = 60
	errs := make(chan error, rounds*2)

	var wg sync.WaitGroup
	wg.Add(2)

	// The reader: the dispatcher's tick and the API's listing.
	go func() {
		defer wg.Done()
		for range rounds {
			for _, n := range channels.configured() {
				// Read a field, so a compiler that decides the copy is
				// unused cannot delete the whole loop.
				_ = n.ID
			}
			channels.Channels()
			channels.diagChannels()
		}
	}()

	// The writer: somebody adding and removing channels from the dashboard.
	go func() {
		defer wg.Done()
		for i := range rounds {
			id := fmt.Sprintf("ops-%d", i)
			if err := channels.Save(api.NotificationChannel{
				ID: id, Kind: "discord", Enabled: true,
			}, target); err != nil {
				errs <- fmt.Errorf("save %s: %w", id, err)
				return
			}
			if err := channels.Remove(id); err != nil {
				errs <- fmt.Errorf("remove %s: %w", id, err)
				return
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The channel that was there before is still there: a test that raced the
	// configuration into an empty file would otherwise pass quietly.
	if list := channels.Channels(); len(list) != 1 || list[0].ID != "ops" {
		t.Errorf("channels after the storm = %+v, want just the one that was there first", list)
	}
}

// --- the API and doctor wiring -----------------------------------------------

// A dependency left out of api.Options does not fail a build and does not fail
// a handler: the routes answer 503 forever, and every test that never asked
// stays green. That already happened once, to the whole plan catalogue.
func TestServeWiresTheNotificationRoutes(t *testing.T) {
	opts := serveAPIOptions(store.Noop{}, &config.Config{}, &cliProviders{},
		&cliNotifications{}, true)

	if opts.Notifications == nil {
		t.Error("Options.Notifications is nil: every notification route would answer 503")
	}
	if opts.Deliveries == nil {
		t.Error("Options.Deliveries is nil: the dashboard could never say a channel stopped working")
	}
	if !opts.NotifyDispatcherOff {
		t.Error("--no-notify does not reach the API: doctor would report alerting as healthy while nothing sends")
	}
}

// The one rule this adapter most needs to get right. The API never hands a URL
// back, so the dashboard cannot prefill the field, and every edit of a kind or
// a toggle arrives with it blank. A blank field that wiped a working webhook
// would take alerting down through the feature meant to protect it.
func TestSavingAChannelWithNoURLKeepsTheStoredOne(t *testing.T) {
	a, _, _, cfgPath := newNotifyApp(t)
	const target = "https://discord.com/api/webhooks/1/SUPERSECRETTOKEN"

	if err := runNotify(a, "add", "ops", "--kind", "discord", "--url", target); err != nil {
		t.Fatalf("notify add: %v", err)
	}

	channels := &cliNotifications{a: a}
	if err := channels.Save(api.NotificationChannel{ID: "ops", Kind: "slack", Enabled: false}, ""); err != nil {
		t.Fatalf("Save with no url: %v", err)
	}

	got, err := channels.Target("ops")
	if err != nil {
		t.Fatalf("the stored url is gone: %v", err)
	}
	if got != target {
		t.Errorf("stored url = %q, want the one that was there before the edit", got)
	}

	// And the edit itself has to have happened, or the rule above is just a
	// method that ignores its argument.
	list := channels.Channels()
	if len(list) != 1 {
		t.Fatalf("got %d channels, want 1", len(list))
	}
	if list[0].Kind != "slack" || list[0].Enabled {
		t.Errorf("the edit was not applied: %+v", list[0])
	}
	if list[0].Host != "discord.com" {
		t.Errorf("Host = %q, want the host and nothing else", list[0].Host)
	}

	// It must be on disk, not only in memory: a process that restarts must
	// find what the dashboard saved.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "kind: slack") {
		t.Errorf("the edit was not written to the configuration:\n%s", raw)
	}
	if strings.Contains(string(raw), "SUPERSECRETTOKEN") {
		t.Error("the webhook url was written in plaintext")
	}
}

func TestSavingANewChannelNeedsAURL(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)
	channels := &cliNotifications{a: a}

	err := channels.Save(api.NotificationChannel{ID: "new", Kind: "discord", Enabled: true}, "")
	if err == nil {
		t.Fatal("a channel with no destination was created; it would never send anything")
	}
	if !strings.Contains(err.Error(), "new") {
		t.Errorf("the error does not name the channel: %v", err)
	}
}

func TestRemovingAChannelThroughTheAPIWritesTheFile(t *testing.T) {
	a, _, _, cfgPath := newNotifyApp(t)
	if err := runNotify(a, "add", "ops", "--kind", "discord",
		"--url", "https://discord.com/api/webhooks/1/tok"); err != nil {
		t.Fatalf("notify add: %v", err)
	}

	channels := &cliNotifications{a: a}
	if err := channels.Remove("ops"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := channels.Remove("ops"); err == nil {
		t.Error("removing the same channel twice reported success the second time")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifications) != 0 {
		t.Fatalf("the channel is still in the file: %+v", cfg.Notifications)
	}
}

// Two diagnostics that disagree about whether alerting works are worse than
// one that is incomplete: an operator who saw the dashboard say nothing is
// wrong will not go and run the command.
func TestDoctorReadsTheNotificationConfiguration(t *testing.T) {
	a, _, _, _ := newNotifyApp(t)
	if err := runNotify(a, "add", "ops", "--kind", "discord",
		"--url", "https://discord.com/api/webhooks/1/tok"); err != nil {
		t.Fatalf("notify add: %v", err)
	}
	cfg, err := a.config()
	if err != nil {
		t.Fatal(err)
	}

	// doctorInput opens the history, so it has to be closed before the
	// temporary directory can be removed on Windows.
	defer func() { _ = a.store(context.Background()).Close() }()
	in := a.doctorInput(context.Background(), cfg, config.Provider{ID: "pve"}, "", true)

	if len(in.Notifications) != 1 || in.Notifications[0].ID != "ops" {
		t.Errorf("doctor was not given the configured channels: %+v", in.Notifications)
	}
	if in.Deliveries == nil {
		t.Error("doctor was given no delivery history: it could never say a channel stopped working")
	}
	if !in.NotifyDispatcherOff {
		t.Error("--no-notify does not reach the diagnostic: it would call a silent installation healthy")
	}
}
