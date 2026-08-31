package proxmox

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// newTestAdminClient wires an AdminClient to the mock server with sane
// defaults every test can override via mutate.
func newTestAdminClient(t *testing.T, m *mockServer, password string, mutate func(*AdminConfig)) *AdminClient {
	t.Helper()
	cfg := AdminConfig{
		Endpoint: m.url(),
		Username: "root@pam",
		Password: password,
		Timeout:  5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewAdminClient(cfg)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	return c
}

// mockTicket registers a canned successful /access/ticket response.
func mockTicket(m *mockServer, ticket, csrf string) {
	m.on("POST", "/api2/json/access/ticket", 200, map[string]any{
		"ticket":              ticket,
		"CSRFPreventionToken": csrf,
		"username":            "root@pam",
	})
}

func mustLogin(t *testing.T, c *AdminClient) {
	t.Helper()
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

// writesOnly filters out reads and the initial ticket exchange, leaving the
// ordered sequence of state-changing requests Bootstrap issued.
func writesOnly(reqs []recordedRequest) []recordedRequest {
	var out []recordedRequest
	for _, r := range reqs {
		if r.Method != "POST" && r.Method != "PUT" {
			continue
		}
		if r.Path == "/api2/json/access/ticket" {
			continue
		}
		out = append(out, r)
	}
	return out
}

func TestLoginSendsCredentialsAndSessionCarriesTicketAndCSRF(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-abc123", "csrf-xyz789")
	m.on("GET", "/api2/json/version", 200, map[string]any{"version": "8.1.4"})
	m.on("PUT", "/api2/json/access/acl", 200, nil)

	c := newTestAdminClient(t, m, "s3cr3t-admin-pw", nil)
	mustLogin(t, c)

	// Ticket call carried username/password as form fields.
	reqs := m.recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request after Login, got %d", len(reqs))
	}
	ticketReq := reqs[0]
	if got := ticketReq.Form.Get("username"); got != "root@pam" {
		t.Errorf("ticket call username = %q, want %q", got, "root@pam")
	}
	if got := ticketReq.Form.Get("password"); got != "s3cr3t-admin-pw" {
		t.Errorf("ticket call password = %q, want %q", got, "s3cr3t-admin-pw")
	}

	// A GET carries the cookie but not the CSRF header.
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	getReq := m.recorded()[1]
	if want := "PVEAuthCookie=tkt-abc123"; getReq.CookieHeader != want {
		t.Errorf("GET Cookie = %q, want %q", getReq.CookieHeader, want)
	}
	if got := getReq.CSRFHeader; got != "" {
		t.Errorf("GET must not carry CSRFPreventionToken, got %q", got)
	}

	// A write carries both the cookie and the CSRF header.
	if _, err := c.doRequest(context.Background(), "PUT", "/access/acl", nil); err != nil {
		t.Fatalf("doRequest PUT: %v", err)
	}
	putReq := m.recorded()[2]
	if want := "PVEAuthCookie=tkt-abc123"; putReq.CookieHeader != want {
		t.Errorf("PUT Cookie = %q, want %q", putReq.CookieHeader, want)
	}
	if got := putReq.CSRFHeader; got != "csrf-xyz789" {
		t.Errorf("PUT CSRFPreventionToken = %q, want %q", got, "csrf-xyz789")
	}
}

func TestLoginUnauthorizedMapsToErrUnauthorizedWithRealmHint(t *testing.T) {
	m := newMockServer(t)
	m.onError("POST", "/api2/json/access/ticket", 401, "authentication failure")

	c := newTestAdminClient(t, m, "wrong-password", func(cfg *AdminConfig) {
		cfg.Username = "root" // missing realm - the common mistake
	})

	err := c.Login(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("expected core.ErrUnauthorized, got %v", err)
	}
	if !strings.Contains(err.Error(), "realm") {
		t.Errorf("expected error to mention the realm requirement, got: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("error must never contain the password, got: %v", err)
	}
}

// emptyClusterOpts is the drill-mode BootstrapOptions used by the
// full-sequence and dry-run tests below.
func emptyClusterOpts() BootstrapOptions {
	return BootstrapOptions{
		UserID:    "restorelab@pve",
		Comment:   "RestoreLab service account",
		TokenName: "drills",
		RoleName:  "RestoreLabDrill",
		Pool:      "drillpool",
		Node:      "",
		Storages:  []string{"local-lvm"},
	}
}

// mockEmptyClusterWrites registers canned responses for an empty cluster
// (no pre-existing roles, pools, users or tokens) that accepts every write
// Bootstrap needs to issue.
func mockEmptyClusterReadsAndWrites(m *mockServer, userID, tokenName string) {
	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{})
	m.on("POST", "/api2/json/access/roles", 200, nil)
	m.on("GET", "/api2/json/pools", 200, []map[string]any{})
	m.on("POST", "/api2/json/pools", 200, nil)
	m.on("GET", "/api2/json/access/users", 200, []map[string]any{})
	m.on("POST", "/api2/json/access/users", 200, nil)
	m.on("PUT", "/api2/json/access/acl", 200, nil)
	m.on("GET", "/api2/json/access/users/"+userID+"/token", 200, []map[string]any{})
	m.on("POST", "/api2/json/access/users/"+userID+"/token/"+tokenName, 200, map[string]any{
		"value":        "e4b6f0e2-secret-token-value",
		"full-tokenid": userID + "!" + tokenName,
	})
}

func TestBootstrapEmptyClusterExactWriteSequence(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if result.TokenID != "restorelab@pve!drills" {
		t.Errorf("TokenID = %q, want %q", result.TokenID, "restorelab@pve!drills")
	}
	if result.Secret != "e4b6f0e2-secret-token-value" {
		t.Errorf("Secret = %q, want %q", result.Secret, "e4b6f0e2-secret-token-value")
	}

	roPrivs := privsKey(ReadOnlyPrivileges)
	drillPrivs := privsKey(DrillPrivileges)
	storagePrivs := privsKey(storagePrivileges)

	writes := writesOnly(m.recorded())
	type want struct {
		method, path string
		form         map[string]string
	}
	wants := []want{
		{"POST", "/api2/json/access/roles", map[string]string{"roleid": "RestoreLabRead", "privs": roPrivs}},
		{"POST", "/api2/json/access/roles", map[string]string{"roleid": "RestoreLabDrill", "privs": drillPrivs}},
		{"POST", "/api2/json/pools", map[string]string{"poolid": "drillpool"}},
		{"POST", "/api2/json/access/users", map[string]string{"userid": "restorelab@pve", "enable": "1"}},
		{"PUT", "/api2/json/access/acl", map[string]string{"path": "/vms", "roles": "RestoreLabRead", "propagate": "1"}},
		{"PUT", "/api2/json/access/acl", map[string]string{"path": "/nodes", "roles": "RestoreLabRead"}},
		{"PUT", "/api2/json/access/acl", map[string]string{"path": "/storage", "roles": "RestoreLabRead"}},
		{"PUT", "/api2/json/access/acl", map[string]string{"path": "/pool/drillpool", "roles": "RestoreLabDrill"}},
		{"POST", "/api2/json/access/roles", map[string]string{"roleid": "RestoreLabDrillStorage", "privs": storagePrivs}},
		{"PUT", "/api2/json/access/acl", map[string]string{"path": "/storage/local-lvm", "roles": "RestoreLabDrillStorage"}},
		{"POST", "/api2/json/access/users/restorelab@pve/token/drills", map[string]string{"privsep": "0"}},
	}

	if len(writes) != len(wants) {
		t.Fatalf("got %d write requests, want %d:\n%+v", len(writes), len(wants), writes)
	}
	for i, w := range wants {
		got := writes[i]
		if got.Method != w.method || got.Path != w.path {
			t.Errorf("write[%d] = %s %s, want %s %s", i, got.Method, got.Path, w.method, w.path)
			continue
		}
		for k, v := range w.form {
			if got.Form.Get(k) != v {
				t.Errorf("write[%d] (%s %s) form[%q] = %q, want %q", i, w.method, w.path, k, got.Form.Get(k), v)
			}
		}
	}

	// Every write must carry the CSRF header.
	for i, r := range writes {
		if r.CSRFHeader != "csrf-1" {
			t.Errorf("write[%d] missing/wrong CSRFPreventionToken: %q", i, r.CSRFHeader)
		}
	}

	// Sanity-check a handful of step statuses.
	wantStatuses := map[string]string{
		"create role RestoreLabRead":         "created",
		"create role RestoreLabDrill":        "created",
		"create pool drillpool":              "created",
		"create user restorelab@pve":         "created",
		"create role RestoreLabDrillStorage": "created",
		"create token restorelab@pve!drills": "created",
	}
	for _, step := range result.Steps {
		if want, ok := wantStatuses[step.Description]; ok && step.Status != want {
			t.Errorf("step %q status = %q, want %q", step.Description, step.Status, want)
		}
	}
}

func TestBootstrapIdempotentOnAlreadyProvisionedCluster(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-2", "csrf-2")
	opts := emptyClusterOpts()
	opts.Storages = nil // keep the storage-role path out of scope for this test

	roPrivs := privsKey(ReadOnlyPrivileges)
	drillPrivs := privsKey(DrillPrivileges)
	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{
		{"roleid": "RestoreLabRead", "privs": roPrivs},
		{"roleid": "RestoreLabDrill", "privs": drillPrivs},
	})
	m.on("GET", "/api2/json/pools", 200, []map[string]any{{"poolid": "drillpool"}})
	m.on("GET", "/api2/json/access/users", 200, []map[string]any{{"userid": "restorelab@pve"}})
	m.on("PUT", "/api2/json/access/acl", 200, nil)
	m.on("GET", "/api2/json/access/users/"+opts.UserID+"/token", 200, []map[string]any{
		{"tokenid": "drills"},
	})
	// Deliberately no POST handlers for /access/roles, /pools or
	// /access/users: if Bootstrap issues any of them, the mock answers 501
	// and the call fails loudly.

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error from the pre-existing token name, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected error about the pre-existing token, got: %v", err)
	}

	for _, r := range m.recorded() {
		if r.Method == "POST" && (r.Path == "/api2/json/access/roles" || r.Path == "/api2/json/pools" || r.Path == "/api2/json/access/users") {
			t.Errorf("unexpected create request on an already-provisioned cluster: %s %s", r.Method, r.Path)
		}
	}

	wantAlready := []string{"create role RestoreLabRead", "create role RestoreLabDrill", "create pool drillpool", "create user restorelab@pve"}
	byDesc := map[string]string{}
	for _, s := range result.Steps {
		byDesc[s.Description] = s.Status
	}
	for _, d := range wantAlready {
		if got := byDesc[d]; got != "already exists" {
			t.Errorf("step %q status = %q, want %q", d, got, "already exists")
		}
	}
	if last := result.Steps[len(result.Steps)-1]; last.Status != "skipped" {
		t.Errorf("final (token) step status = %q, want %q", last.Status, "skipped")
	}
}

func TestBootstrapReadOnlyGrantsNoWritePrivilege(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-3", "csrf-3")
	opts := BootstrapOptions{
		UserID:    "restorelab-ro@pve",
		TokenName: "audit",
		RoleName:  "RestoreLabRead",
		ReadOnly:  true,
	}
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if _, err := c.Bootstrap(context.Background(), opts); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Datastore.AllocateSpace is deliberately absent from this list: Proxmox
	// will not show a backup volume without it, so read-only discovery is
	// impossible without it. See the comment on ReadOnlyPrivileges. What must
	// never appear is anything that can restore, power, reconfigure or delete.
	forbidden := []string{
		"VM.Allocate",
		"VM.PowerMgmt",
		"VM.Config.",
		"VM.GuestAgent.Unrestricted",
		"Datastore.Allocate,", // the volume-deleting one, not AllocateSpace
		"Sys.Modify",
		"Permissions.Modify",
	}
	for _, r := range m.recorded() {
		blob := r.Form.Encode() + " " + r.Query.Encode()
		decoded, err := url.QueryUnescape(blob)
		if err == nil {
			blob = decoded
		}
		for _, priv := range forbidden {
			if strings.Contains(blob, priv) {
				t.Errorf("ReadOnly bootstrap must never send %s: %s %s form=%v", priv, r.Method, r.Path, r.Form)
			}
		}
	}

	// Only one role should ever be touched in ReadOnly mode.
	for _, r := range m.recorded() {
		if r.Method == "POST" && r.Path == "/api2/json/access/roles" {
			if got := r.Form.Get("roleid"); got != "RestoreLabRead" {
				t.Errorf("unexpected role created in ReadOnly mode: %q", got)
			}
		}
	}
}

func TestBootstrapNoPoolRecordsWarningStep(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-4", "csrf-4")
	opts := BootstrapOptions{
		UserID:    "restorelab@pve",
		TokenName: "drills",
		RoleName:  "RestoreLabDrill",
		// Pool intentionally left empty.
	}
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var found bool
	for _, s := range result.Steps {
		if s.Description == "grant RestoreLabDrill on /vms" {
			found = true
			if !strings.Contains(strings.ToLower(s.Detail), "destroy") || !strings.Contains(s.Detail, "cluster") {
				t.Errorf("warning step Detail does not read as a warning: %q", s.Detail)
			}
		}
	}
	if !found {
		t.Error("expected a step granting the drill role directly on /vms with a warning Detail")
	}
}

func TestBootstrapDryRunIssuesZeroWrites(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-5", "csrf-5")
	opts := emptyClusterOpts()
	opts.DryRun = true

	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{})
	m.on("GET", "/api2/json/pools", 200, []map[string]any{})
	m.on("GET", "/api2/json/access/users", 200, []map[string]any{})
	m.on("GET", "/api2/json/access/users/"+opts.UserID+"/token", 200, []map[string]any{})
	// No POST/PUT handlers registered at all: any write attempt 501s.

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap (dry run): %v", err)
	}
	if result.Secret != "" {
		t.Errorf("dry run must not produce a real secret, got %q", result.Secret)
	}
	if want := opts.UserID + "!" + opts.TokenName; result.TokenID != want {
		t.Errorf("dry run TokenID = %q, want %q", result.TokenID, want)
	}
	for _, r := range m.recorded() {
		if r.Method == "POST" || r.Method == "PUT" {
			if r.Path == "/api2/json/access/ticket" {
				continue // Login itself is unavoidable and precedes DryRun
			}
			t.Errorf("dry run issued a write: %s %s", r.Method, r.Path)
		}
	}
	for _, s := range result.Steps {
		if s.Status != "would create" {
			t.Errorf("step %q status = %q, want %q under DryRun", s.Description, s.Status, "would create")
		}
	}
}

func TestBootstrapNeverLeaksPasswordOrSecret(t *testing.T) {
	const password = "s3cr3t-admin-pw-do-not-leak"

	m := newMockServer(t)
	mockTicket(m, "tkt-6", "csrf-6")
	opts := emptyClusterOpts()
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)

	cfg := AdminConfig{Endpoint: m.url(), Username: "root@pam", Password: password, Timeout: 5 * time.Second}
	c, err := NewAdminClient(cfg)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if strings.Contains(cfg.String(), password) {
		t.Errorf("AdminConfig.String() leaked the password: %s", cfg.String())
	}
	if strings.Contains(result.String(), password) {
		t.Errorf("BootstrapResult.String() leaked the password: %s", result.String())
	}
	if strings.Contains(result.String(), result.Secret) {
		t.Errorf("BootstrapResult.String() leaked the token secret: %s", result.String())
	}
	for _, s := range result.Steps {
		if strings.Contains(s.Description, password) || strings.Contains(s.Detail, password) {
			t.Errorf("a BootstrapStep leaked the password: %+v", s)
		}
		if result.Secret != "" && (strings.Contains(s.Description, result.Secret) || strings.Contains(s.Detail, result.Secret)) {
			t.Errorf("a BootstrapStep leaked the token secret: %+v", s)
		}
	}

	// Also exercise an error path: a duplicate token name must not leak
	// the password either.
	m2 := newMockServer(t)
	mockTicket(m2, "tkt-7", "csrf-7")
	m2.on("GET", "/api2/json/access/roles", 200, []map[string]any{
		{"roleid": "RestoreLabRead", "privs": privsKey(ReadOnlyPrivileges)},
		{"roleid": "RestoreLabDrill", "privs": privsKey(DrillPrivileges)},
	})
	m2.on("GET", "/api2/json/pools", 200, []map[string]any{{"poolid": "drillpool"}})
	m2.on("GET", "/api2/json/access/users", 200, []map[string]any{{"userid": opts.UserID}})
	m2.on("PUT", "/api2/json/access/acl", 200, nil)
	m2.on("GET", "/api2/json/access/users/"+opts.UserID+"/token", 200, []map[string]any{{"tokenid": "drills"}})

	c2 := newTestAdminClient(t, m2, password, nil)
	mustLogin(t, c2)
	_, err = c2.Bootstrap(context.Background(), opts)
	if err == nil {
		t.Fatal("expected the duplicate-token error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("bootstrap error leaked the password: %v", err)
	}

	// Close wipes the in-memory secrets; document (not strictly testable
	// from outside the package) that the fields are reset.
	c2.Close()
	if c2.password != "" || c2.ticket != "" || c2.csrfToken != "" {
		t.Error("Close did not clear password/ticket/csrfToken")
	}
}

// pve9Roles is the privilege vocabulary of a Proxmox VE 9 cluster as seen
// through its built-in roles: VM.Monitor is gone, the VM.GuestAgent.* pair is
// present. Only the privileges RestoreLab asks for are listed; a real cluster
// has many more, which is irrelevant to the filtering.
var pve9Roles = map[string]any{
	"roleid": "Administrator",
	"privs": strings.Join([]string{
		"VM.Audit", "VM.Backup", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted",
		"Datastore.Audit", "Datastore.AllocateSpace", "Sys.Audit",
		"VM.Allocate", "VM.Config.CPU", "VM.Config.Disk", "VM.Config.HWType",
		"VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt",
	}, ","),
}

// The privilege list is not stable across Proxmox major versions. Asking for
// one that this cluster does not know fails the whole role creation with a
// parameter verification error, so RestoreLab must adapt to the vocabulary it
// finds rather than to the one it was written against.
func TestBootstrapDropsPrivilegesTheClusterDoesNotKnow(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)
	// Override the empty role list with a PVE 9 vocabulary.
	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{pve9Roles})

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap must succeed on Proxmox VE 9: %v", err)
	}

	for _, r := range writesOnly(m.recorded()) {
		if r.Path != "/api2/json/access/roles" {
			continue
		}
		privs := r.Form.Get("privs")
		if strings.Contains(privs, "VM.Monitor") {
			t.Errorf("VM.Monitor must not be sent to a cluster that does not know it: %q", privs)
		}
		// The auxiliary storage role is Datastore-only by design.
		if strings.HasSuffix(r.Form.Get("roleid"), "Storage") {
			continue
		}
		if !strings.Contains(privs, "VM.Audit") {
			t.Errorf("supported privileges must still be sent: %q", privs)
		}
	}

	var explained bool
	for _, step := range result.Steps {
		if strings.Contains(step.Detail, "VM.Monitor") && strings.Contains(step.Detail, "not supported") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the dropped privilege must be reported to the user, got steps %+v", result.Steps)
	}
}

// Dropping an optional privilege is adaptation; dropping one RestoreLab cannot
// work without is a broken assumption, and must stop rather than quietly
// create a role that cannot do its job.
func TestBootstrapFailsWhenARequiredPrivilegeIsUnknown(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)
	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{
		{"roleid": "Administrator", "privs": "VM.Backup,Sys.Audit,Datastore.Audit"},
	})

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	_, err := c.Bootstrap(context.Background(), opts)
	if err == nil {
		t.Fatal("Bootstrap() error = nil, want a refusal when VM.Audit is unknown")
	}
	if !strings.Contains(err.Error(), "VM.Audit") {
		t.Errorf("the error must name the missing privilege, got %v", err)
	}
	for _, r := range writesOnly(m.recorded()) {
		if r.Path == "/api2/json/access/users" {
			t.Error("nothing may be created once the privilege check has failed")
		}
	}
}

// Proxmox hides backup volumes from a storage content listing unless the
// caller can allocate space on that storage: Datastore.Audit and VM.Backup are
// not enough, verified against Proxmox VE 9.2.3. Discovery is the whole point
// of the read-only mode, so losing this privilege would make it useless.
func TestReadOnlyPrivilegesCanSeeBackupVolumes(t *testing.T) {
	var hasAudit, hasAllocateSpace, hasBackup bool
	for _, p := range ReadOnlyPrivileges {
		switch p {
		case "Datastore.Audit":
			hasAudit = true
		case "Datastore.AllocateSpace":
			hasAllocateSpace = true
		case "VM.Backup":
			hasBackup = true
		case "Datastore.Allocate":
			t.Error("Datastore.Allocate allows deleting volumes and must never be granted to RestoreLab")
		}
	}
	if !hasAudit || !hasAllocateSpace || !hasBackup {
		t.Errorf("read-only privileges cannot list backups: audit=%v allocateSpace=%v backup=%v",
			hasAudit, hasAllocateSpace, hasBackup)
	}
}

// Proxmox ACLs do not accumulate down a path: a grant on /storage/local
// replaces the one inherited from /storage. The per-storage role must
// therefore carry everything needed there, or the narrower grant silently
// removes privileges the broader one provided.
func TestStorageRoleIsSelfSufficient(t *testing.T) {
	needed := map[string]bool{"Datastore.Audit": false, "Datastore.AllocateSpace": false}
	for _, p := range storagePrivileges {
		if _, ok := needed[p]; ok {
			needed[p] = true
		}
	}
	for priv, present := range needed {
		if !present {
			t.Errorf("the per-storage role omits %q, which the inherited /storage grant will not make up for", priv)
		}
	}
}

// Proxmox VE 9 routes every bridge access through the SDN permission tree, so
// attaching a restored workload even to a plain Linux bridge is refused with
// 403 SDN.Use without this grant. It is scoped to the one bridge drills use:
// the difference between "may use the recovery network" and "may attach a
// workload to any network on this cluster".
func TestBootstrapGrantsSDNUseOnTheIsolatedBridgeOnly(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	opts.Bridge = "vmbr99"
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)
	m.on("GET", "/api2/json/access/roles", 200, []map[string]any{pve9Roles})

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if _, err := c.Bootstrap(context.Background(), opts); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var granted bool
	for _, r := range writesOnly(m.recorded()) {
		if r.Path != "/api2/json/access/acl" {
			continue
		}
		path := r.Form.Get("path")
		if path == "/sdn/zones/localnetwork/vmbr99" {
			granted = true
		}
		if path == "/sdn" || path == "/sdn/zones" || path == "/sdn/zones/localnetwork" {
			t.Errorf("SDN.Use must be scoped to the bridge, not to %q", path)
		}
	}
	if !granted {
		t.Error("no SDN.Use grant on the isolated bridge; a restore would be refused with 403")
	}
}

func TestBootstrapSkipsTheSDNGrantWithoutABridge(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts() // no Bridge
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if _, err := c.Bootstrap(context.Background(), opts); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, r := range writesOnly(m.recorded()) {
		if r.Path == "/api2/json/access/acl" && strings.HasPrefix(r.Form.Get("path"), "/sdn") {
			t.Errorf("no SDN grant should be made when no bridge was given, got %q", r.Form.Get("path"))
		}
	}
}

// Re-running a bootstrap must be able to reconcile roles and ACLs without
// destroying a token that already works, since Proxmox reveals a secret only
// at creation.
func TestBootstrapReusesAnExistingTokenWhenAsked(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	opts.ReuseExistingToken = true
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)
	m.on("GET", "/api2/json/access/users/"+opts.UserID+"/token", 200, []map[string]any{
		{"tokenid": opts.TokenName},
	})

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	result, err := c.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap with an existing token must succeed when reuse was asked: %v", err)
	}
	if result.Secret != "" {
		t.Error("Proxmox cannot reveal an existing secret; none must be invented")
	}
	if result.TokenID != opts.UserID+"!"+opts.TokenName {
		t.Errorf("TokenID = %q", result.TokenID)
	}
	for _, r := range writesOnly(m.recorded()) {
		if strings.Contains(r.Path, "/token/") {
			t.Errorf("an existing token must not be recreated, got %s %s", r.Method, r.Path)
		}
	}
}

// Without the reuse flag, an existing token still stops the run: silently
// carrying on would leave the caller with a token whose secret it does not
// have.
func TestBootstrapStillRefusesAnExistingTokenByDefault(t *testing.T) {
	m := newMockServer(t)
	mockTicket(m, "tkt-1", "csrf-1")
	opts := emptyClusterOpts()
	mockEmptyClusterReadsAndWrites(m, opts.UserID, opts.TokenName)
	m.on("GET", "/api2/json/access/users/"+opts.UserID+"/token", 200, []map[string]any{
		{"tokenid": opts.TokenName},
	})

	c := newTestAdminClient(t, m, "admin-pw", nil)
	mustLogin(t, c)

	if _, err := c.Bootstrap(context.Background(), opts); err == nil {
		t.Fatal("Bootstrap() error = nil, want a refusal on a pre-existing token")
	}
}
