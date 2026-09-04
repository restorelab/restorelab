package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/crypto"
)

func mustKey(t *testing.T) crypto.Key {
	t.Helper()
	k, err := crypto.NewKey()
	if err != nil {
		t.Fatalf("crypto.NewKey: %v", err)
	}
	return k
}

// validConfig returns a Config that passes Validate() and Save(), with one
// sealed provider secret.
func validConfig(t *testing.T, k crypto.Key) *Config {
	t.Helper()
	c := New()
	c.Providers = []Provider{{
		ID:       "proxmox-main",
		Kind:     "proxmox",
		Roles:    []string{"hypervisor"},
		Endpoint: "https://pve.example.com:8006",
		TokenID:  "root@pam!restorelab",
		Node:     "pve1",
	}}
	if err := c.SetProviderSecret("proxmox-main", "s3cr3t-token-value", k); err != nil {
		t.Fatalf("SetProviderSecret: %v", err)
	}
	c.Defaults.Network = "isolated"
	return c
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	k := mustKey(t)

	c := validConfig(t, k)
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != c.Version {
		t.Fatalf("Version = %d, want %d", loaded.Version, c.Version)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].ID != "proxmox-main" {
		t.Fatalf("providers not round-tripped correctly: %+v", loaded.Providers)
	}
	if !crypto.IsSealed(loaded.Providers[0].TokenSecret) {
		t.Fatalf("loaded TokenSecret is not sealed: %q", loaded.Providers[0].TokenSecret)
	}
	secret, err := loaded.Providers[0].Secret(k)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret != "s3cr3t-token-value" {
		t.Fatalf("Secret() = %q, want %q", secret, "s3cr3t-token-value")
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "does-not-exist.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing file: err = %v, want ErrNotFound", err)
	}
}

func TestLoadStrictDecodingRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	bad := "version: 1\nnot_a_real_field: true\n"
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("Load with unknown field succeeded, want error")
	}
}

func TestSaveRefusesUnsealedSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	c := New()
	c.Providers = []Provider{{
		ID:          "proxmox-main",
		Kind:        "proxmox",
		Roles:       []string{"hypervisor"},
		Endpoint:    "https://pve.example.com:8006",
		TokenID:     "root@pam!restorelab",
		TokenSecret: "plaintext-oops",
	}}

	err := Save(path, c)
	if err == nil {
		t.Fatalf("Save with unsealed secret succeeded, want error")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("Save left a file behind despite refusing: %v", statErr)
	}
}

func TestProviderSecretRejectsPlaintext(t *testing.T) {
	k := mustKey(t)
	p := Provider{ID: "x", TokenSecret: "plaintext-not-sealed"}
	if _, err := p.Secret(k); err == nil {
		t.Fatalf("Secret() on plaintext TokenSecret succeeded, want error")
	}
}

func TestDefaultPathHonoursEnvVar(t *testing.T) {
	t.Setenv("RESTORELAB_CONFIG", "/custom/path/config.yaml")
	if got := DefaultPath(); got != "/custom/path/config.yaml" {
		t.Fatalf("DefaultPath() = %q, want the env override", got)
	}
}

func TestDefaultPathFallsBackToHome(t *testing.T) {
	t.Setenv("RESTORELAB_CONFIG", "")
	os.Unsetenv("RESTORELAB_CONFIG")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got := DefaultPath()
	want := filepath.Join(home, ".restorelab", "config.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestValidateDuplicateProviderIDs(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "dup", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
		{ID: "dup", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://b.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate provider id") {
		t.Fatalf("Validate() = %v, want duplicate provider id error", err)
	}
}

func TestValidateBadKind(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "vmware", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Validate() = %v, want unsupported kind error", err)
	}
}

func TestValidateBadRole(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"database"}, Endpoint: "https://a.example.com"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `role "database"`) {
		t.Fatalf("Validate() = %v, want bad role error", err)
	}
}

func TestValidateMissingNetworkProfile(t *testing.T) {
	c := New()
	c.Defaults.Network = "does-not-exist"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not name a network profile") {
		t.Fatalf("Validate() = %v, want missing network profile error", err)
	}
}

func TestValidateNonIsolatedDefaultNetwork(t *testing.T) {
	c := New()
	c.Networks["prod"] = Network{Bridge: "vmbr0", Isolated: false}
	c.Defaults.Network = "prod"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "must never be the default") {
		t.Fatalf("Validate() = %v, want non-isolated default network error", err)
	}
}

func TestValidateBadEndpoint(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "not-a-url"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("Validate() = %v, want bad endpoint error", err)
	}
}

func TestValidateTempIDRange(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "x", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com", TempIDMin: 900, TempIDMax: 800},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "temp_id_min must be less than temp_id_max") {
		t.Fatalf("Validate() = %v, want temp id range error", err)
	}
}

func TestValidateAccumulatesAllErrors(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "", Kind: "bogus", Roles: []string{"nope"}, Endpoint: "not-a-url"},
	}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate() succeeded, want multiple errors")
	}
	msg := err.Error()
	for _, want := range []string{"id is required", "not supported", `role "nope"`, "not a valid URL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Validate() error missing %q; got:\n%s", want, msg)
		}
	}
}

func TestResolveNetwork(t *testing.T) {
	c := New()
	nc, err := c.ResolveNetwork("isolated")
	if err != nil {
		t.Fatalf("ResolveNetwork: %v", err)
	}
	if nc.Bridge != "vmbr99" || !nc.Isolated {
		t.Fatalf("ResolveNetwork(isolated) = %+v, want bridge vmbr99, isolated true", nc)
	}

	if _, err := c.ResolveNetwork("missing"); err == nil {
		t.Fatalf("ResolveNetwork(missing) succeeded, want error")
	}
}

func TestProvidersByRole(t *testing.T) {
	c := New()
	c.Providers = []Provider{
		{ID: "hv", Kind: "proxmox", Roles: []string{"hypervisor"}, Endpoint: "https://a.example.com"},
		{ID: "bk", Kind: "pbs", Roles: []string{"backup"}, Endpoint: "https://b.example.com"},
		{ID: "both", Kind: "proxmox", Roles: []string{"hypervisor", "backup"}, Endpoint: "https://c.example.com"},
	}
	hv := c.ProvidersByRole("hypervisor")
	if len(hv) != 2 {
		t.Fatalf("ProvidersByRole(hypervisor) returned %d providers, want 2", len(hv))
	}
	bk := c.ProvidersByRole("backup")
	if len(bk) != 2 {
		t.Fatalf("ProvidersByRole(backup) returned %d providers, want 2", len(bk))
	}
}

func TestUpsertReplacesByID(t *testing.T) {
	c := New()
	c.Upsert(Provider{ID: "x", Endpoint: "https://one.example.com"})
	c.Upsert(Provider{ID: "y", Endpoint: "https://two.example.com"})
	c.Upsert(Provider{ID: "x", Endpoint: "https://updated.example.com"})

	if len(c.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(c.Providers))
	}
	p, err := c.Provider("x")
	if err != nil {
		t.Fatalf("Provider(x): %v", err)
	}
	if p.Endpoint != "https://updated.example.com" {
		t.Fatalf("Upsert did not replace: Endpoint = %q", p.Endpoint)
	}
}

func TestRedactedAndString(t *testing.T) {
	k := mustKey(t)
	sealed, err := crypto.Seal(k, "top-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	p := Provider{ID: "x", TokenSecret: sealed}

	r := p.Redacted()
	if r.TokenSecret != "***" {
		t.Fatalf("Redacted().TokenSecret = %q, want ***", r.TokenSecret)
	}
	if strings.Contains(p.String(), sealed) {
		t.Fatalf("String() leaked the sealed secret value")
	}
	if strings.Contains(p.String(), "top-secret") {
		t.Fatalf("String() leaked the plaintext secret value")
	}
}

func TestSaveFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are advisory on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	k := mustKey(t)
	c := validConfig(t, k)

	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	k := mustKey(t)
	c := validConfig(t, k)

	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory contains unexpected entries after Save: %v", names)
	}
}

// An absent scheduler block must mean enabled. A plain bool would have made
// the zero value "disabled", and every configuration written before the
// scheduler existed would have silently stopped drilling.
func TestSchedulerIsEnabledWhenTheBlockIsAbsent(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("version: 1\n"), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !c.SchedulerEnabled() {
		t.Fatal("a config with no scheduler block must still schedule")
	}
}

func TestSchedulerCanBeTurnedOff(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("version: 1\nscheduler:\n  enabled: false\n"), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.SchedulerEnabled() {
		t.Fatal("enabled: false must turn the scheduler off")
	}
}

func TestSchedulerReadsItsDurations(t *testing.T) {
	var c Config
	doc := "version: 1\nscheduler:\n  grace_period: 30m\n  max_queue_depth: 12\n"
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.Scheduler.GracePeriod != 30*time.Minute {
		t.Fatalf("GracePeriod = %v, want 30m", c.Scheduler.GracePeriod)
	}
	if c.Scheduler.MaxQueueDepth != 12 {
		t.Fatalf("MaxQueueDepth = %d, want 12", c.Scheduler.MaxQueueDepth)
	}
}

// A Discord webhook URL is a bearer credential: whoever holds it can post
// into that channel, with no second factor. Invariant 8 covers it exactly as
// it covers a provider token, so Save has to refuse the same way.
func TestSaveRefusesAnUnsealedWebhookURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	c := New()
	c.Notifications = []Notification{{
		ID:   "ops",
		Kind: "discord",
		URL:  "https://discord.com/api/webhooks/1/abc",
	}}

	err := Save(path, c)
	if err == nil {
		t.Fatal("Save wrote a plaintext webhook URL to disk")
	}
	if !strings.Contains(err.Error(), "ops") {
		t.Errorf("error does not name the offending channel: %v", err)
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error does not say which field is unsealed, so the reader cannot tell it from a provider refusal: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("Save left a file behind despite refusing: %v", statErr)
	}
}

func TestSetNotificationURLSealsIt(t *testing.T) {
	k := mustKey(t)
	const plaintext = "https://discord.com/api/webhooks/1/abc"

	c := New()
	c.UpsertNotification(Notification{ID: "ops", Kind: "discord"})
	if err := c.SetNotificationURL("ops", plaintext, k); err != nil {
		t.Fatalf("SetNotificationURL: %v", err)
	}

	n, err := c.Notification("ops")
	if err != nil {
		t.Fatalf("Notification: %v", err)
	}
	if !crypto.IsSealed(n.URL) {
		t.Fatalf("stored url is not sealed: %q", n.URL)
	}
	if strings.Contains(n.URL, "abc") {
		t.Fatalf("the sealed url still carries the plaintext: %q", n.URL)
	}

	got, err := n.Target(k)
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Target() = %q, want %q", got, plaintext)
	}

	// Target must refuse an unsealed value rather than hand it back, for the
	// same reason Provider.Secret does: a plaintext url in that field means
	// something bypassed the sealing path.
	plain := Notification{ID: "hand-edited", URL: plaintext}
	if _, err := plain.Target(k); err == nil {
		t.Fatal("Target() on an unsealed url succeeded, want error")
	}

	// A %v of a channel must never be able to leak the url.
	if strings.Contains(n.String(), n.URL) || strings.Contains(n.String(), plaintext) {
		t.Fatalf("String() leaked the url: %s", n.String())
	}
	if r := n.Redacted(); r.URL == n.URL || strings.Contains(r.URL, "abc") {
		t.Fatalf("Redacted() kept the url: %q", r.URL)
	}

	if err := c.SetNotificationURL("nope", plaintext, k); err == nil {
		t.Fatal("SetNotificationURL on an unknown channel succeeded, want error")
	}
}

// An absent enabled field must mean on. A plain bool would make the zero
// value "off", and a channel added by hand in YAML would silently never fire.
func TestAnAbsentEnabledMeansOn(t *testing.T) {
	var c Config
	doc := "version: 1\nnotifications:\n  - id: ops\n    kind: discord\n    url: rlsec:v1:x\n"
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(c.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d, want 1", len(c.Notifications))
	}
	if !c.Notifications[0].On() {
		t.Fatal("a channel with no enabled field must still fire")
	}

	off := false
	if (Notification{Enabled: &off}).On() {
		t.Fatal("enabled: false must turn the channel off")
	}
	on := true
	if !(Notification{Enabled: &on}).On() {
		t.Fatal("enabled: true must leave the channel on")
	}
}

func TestValidateRejectsAnUnknownKind(t *testing.T) {
	c := New()
	c.Notifications = []Notification{{
		ID:   "ops",
		Kind: "telegram",
		URL:  "https://example.com/hook",
	}}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unknown notification kind")
	}
	msg := err.Error()
	if !strings.Contains(msg, "notifications[0] (ops)") {
		t.Errorf("error does not locate the channel: %s", msg)
	}
	// The message has to name the kinds that do exist, or the reader is left
	// guessing which three words are legal.
	for _, want := range []string{"discord", "slack", "webhook"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name the valid kind %q; got:\n%s", want, msg)
		}
	}
}

func TestValidateRejectsABadNotificationBlock(t *testing.T) {
	c := New()
	c.Notifications = []Notification{
		{ID: "", Kind: "discord", URL: "https://example.com/hook"},
		{ID: "dup", Kind: "slack", URL: ""},
		{ID: "dup", Kind: "webhook", URL: "https://example.com/hook"},
		{ID: "cleartext", Kind: "webhook", URL: "http://example.com/hook"},
		{ID: "loopback", Kind: "webhook", URL: "http://127.0.0.1:9000/hook"},
		{ID: "localhost", Kind: "webhook", URL: "http://localhost:9000/hook"},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a broken notifications block")
	}
	msg := err.Error()
	for _, want := range []string{
		"notifications[0]: id is required",
		"duplicate notification id",
		"notifications[1] (dup): url is required",
		"notifications[3] (cleartext)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q; got:\n%s", want, msg)
		}
	}
	// A webhook on a loopback host is how somebody tests a receiver on their
	// own machine, and nothing on the wire can read it.
	for _, unwanted := range []string{"(loopback)", "(localhost)"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("Validate() rejected a loopback receiver: %s", msg)
		}
	}
}

func TestValidateChecksTheBaseURL(t *testing.T) {
	c := New()
	c.Server.BaseURL = "dashboard.example.com"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("Validate() = %v, want a server.base_url error", err)
	}

	c.Server.BaseURL = "https://dashboard.example.com"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected a good base_url: %v", err)
	}
}

// TestSetNotificationURLRefusesPlainHTTP guards the order callers actually
// work in. Sealing happens before Save, so a scheme check that only lived in
// Validate would inspect ciphertext and pass everything: the rule has to bite
// at the moment the URL is still readable.
func TestSetNotificationURLRefusesPlainHTTP(t *testing.T) {
	k, err := crypto.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	c := New()
	c.UpsertNotification(Notification{ID: "ops", Kind: "discord"})

	if err := c.SetNotificationURL("ops", "http://hooks.example.com/ingest/abc", k); err == nil {
		t.Fatal("SetNotificationURL sealed a bearer credential destined for plain http")
	}

	// Loopback stays allowed: nothing on a wire can read it, and it is how
	// somebody tries a receiver of their own first.
	if err := c.SetNotificationURL("ops", "http://127.0.0.1:9000/hook", k); err != nil {
		t.Errorf("SetNotificationURL refused a loopback receiver: %v", err)
	}
}
