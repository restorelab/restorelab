package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// ---------------------------------------------------------------------------
// This file implements the six-command SSH ritual ("create a role, create a
// user, grant ACLs, mint a token") as a single Bootstrap call. An
// administrator's credentials are used exactly once, in memory, to build a
// least-privilege service account and API token; RestoreLab never persists
// or logs the admin password, and the resulting token is scoped as tightly
// as the caller's options allow. See AdminClient.Close for what "in memory"
// actually guarantees (and does not) in Go.
// ---------------------------------------------------------------------------

// AdminConfig is a short-lived administrative connection: enough to log in
// once and provision RestoreLab's own service account, never to be reused
// or persisted beyond that.
type AdminConfig struct {
	Endpoint           string // https://pve.example.com:8006
	Username           string // "root@pam"
	Password           string
	InsecureSkipVerify bool
	CACertPEM          string
	Timeout            time.Duration
}

// String redacts Password so AdminConfig is safe to include in logs or
// error messages (e.g. via %v/%s or fmt.Stringer-aware loggers).
func (c AdminConfig) String() string {
	return fmt.Sprintf(
		"AdminConfig{Endpoint:%q, Username:%q, Password:\"[REDACTED]\", InsecureSkipVerify:%v, Timeout:%s}",
		c.Endpoint, c.Username, c.InsecureSkipVerify, c.Timeout,
	)
}

// AdminClient holds a ticket-authenticated PVE session used only to run
// Bootstrap (and, incidentally, Version as a connectivity check). It is not
// a general-purpose API client: Provider (client.go) is what RestoreLab uses
// for everyday operation, authenticated with the token Bootstrap mints.
type AdminClient struct {
	endpoint string
	hc       *http.Client

	// mu guards every field below, all of which are sensitive or mutated
	// after construction: username/password are needed to (re-)authenticate,
	// ticket/csrfToken are the session credentials that authentication
	// produces. Close zeroes all four.
	mu        sync.Mutex
	username  string
	password  string
	ticket    string
	csrfToken string
}

// NewAdminClient validates cfg and prepares a session; it performs no
// network I/O itself. Call Login before anything else.
func NewAdminClient(cfg AdminConfig) (*AdminClient, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("proxmox: AdminConfig.Endpoint is required")
	}
	if cfg.Username == "" {
		return nil, errors.New("proxmox: AdminConfig.Username is required")
	}
	if cfg.Password == "" {
		return nil, errors.New("proxmox: AdminConfig.Password is required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CACertPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, errors.New("proxmox: AdminConfig.CACertPEM does not contain a valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}

	return &AdminClient{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		hc: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		username: cfg.Username,
		password: cfg.Password,
	}, nil
}

// Login exchanges username/password for a PVEAuthCookie ticket and a CSRF
// prevention token via POST /access/ticket. It is the only call that ever
// sends the password over the wire.
func (c *AdminClient) Login(ctx context.Context) error {
	c.mu.Lock()
	username, password := c.username, c.password
	c.mu.Unlock()

	raw, err := c.doRequest(ctx, http.MethodPost, "/access/ticket", url.Values{
		"username": {username},
		"password": {password},
	})
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) {
			// The single most common mistake: PVE usernames are
			// realm-qualified ("root@pam"), and a bare "root" fails auth
			// with the same 401 as a wrong password.
			return fmt.Errorf(
				"proxmox: login failed for user %q: %w (the username must include its realm, e.g. \"root@pam\" rather than \"root\")",
				username, err,
			)
		}
		return err
	}

	var data struct {
		Ticket              string `json:"ticket"`
		CSRFPreventionToken string `json:"CSRFPreventionToken"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("proxmox: decode ticket response: %w", err)
	}
	if data.Ticket == "" {
		return errors.New("proxmox: ticket response did not include a ticket")
	}

	c.mu.Lock()
	c.ticket = data.Ticket
	c.csrfToken = data.CSRFPreventionToken
	c.mu.Unlock()
	return nil
}

// Version returns the PVE version string (e.g. "8.1.4"), primarily useful
// as a cheap reachability check once logged in.
func (c *AdminClient) Version(ctx context.Context) (string, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("proxmox: decode version response: %w", err)
	}
	return asString(data["version"]), nil
}

// Close discards the in-memory ticket, CSRF token and password.
//
// Honest caveat: Go strings are immutable and the runtime gives no way to
// scrub arbitrary heap memory, so reassigning these fields to "" only
// removes AdminClient's own reference to the data - it does not guarantee
// the original bytes are overwritten in process memory (the GC may have
// already copied or relocated them, and other references, e.g. from a
// completed HTTP request's internal buffers, are outside our control).
// This is best-effort hygiene, not a security boundary.
func (c *AdminClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password = ""
	c.ticket = ""
	c.csrfToken = ""
}

func (c *AdminClient) loggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ticket != ""
}

func (c *AdminClient) authHeaders() (cookie, csrf string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ticket, c.csrfToken
}

// doRequest performs one PVE API call authenticated via ticket cookie
// instead of Provider's API-token header. It deliberately mirrors
// Provider.request in client.go and reuses that file's pveEnvelope,
// errBodyTruncateLen and mapStatusError so the two auth paths stay
// consistent and error messages never grow request bodies (which could
// otherwise leak, e.g., form-encoded content) unbounded.
func (c *AdminClient) doRequest(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	reqURL := c.endpoint + "/api2/json" + path

	var bodyReader io.Reader
	switch method {
	case http.MethodGet, http.MethodDelete:
		if len(form) > 0 {
			reqURL += "?" + form.Encode()
		}
	default:
		bodyReader = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("proxmox: build request %s %s: %w", method, path, err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	cookie, csrf := c.authHeaders()
	if cookie != "" {
		req.Header.Set("Cookie", "PVEAuthCookie="+cookie)
	}
	// PVE requires the CSRF token on every state-changing request (POST,
	// PUT, DELETE) once a ticket cookie is in play; a GET must not carry it.
	if csrf != "" && method != http.MethodGet {
		req.Header.Set("CSRFPreventionToken", csrf)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, core.Retryable(fmt.Errorf("proxmox: %s %s: %w", method, path, err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if readErr != nil {
		return nil, core.Retryable(fmt.Errorf("proxmox: reading response body for %s %s: %w", method, path, readErr))
	}

	if resp.StatusCode >= 400 {
		return nil, mapStatusError(resp.StatusCode, method, path, raw)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var env pveEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("proxmox: decode response for %s %s: %w", method, path, err)
	}
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// Privilege sets. Exported and documented because the setup documentation
// quotes them verbatim - anyone auditing what a RestoreLab token can do
// should be able to read this list without reverse-engineering the code.
// ---------------------------------------------------------------------------

// ReadOnlyPrivileges is granted to a read-only RestoreLab service account:
// enough to inspect cluster, VM/CT and backup state without being able to
// change or destroy anything.
//   - VM.Audit                view VM/CT configuration and status
//   - VM.Backup               read a workload's backup catalogue
//   - VM.GuestAgent.Audit     query the QEMU guest agent (guest readiness, IP discovery, OS detection)
//   - Datastore.Audit         view storage contents and usage
//   - Datastore.AllocateSpace see backup volumes at all (see below)
//   - Sys.Audit               view node/cluster health, bridges and capacity
//   - VM.Monitor              read VM runtime state (Proxmox VE 8 and older only)
//
// Datastore.AllocateSpace is not an oversight and not read-only. Proxmox
// filters the storage content listing per volume, and on a directory storage a
// backup volume stays invisible with Datastore.Audit and VM.Backup alone -
// verified against Proxmox VE 9.2.3, where the same request returned the ISOs
// on that storage and silently omitted the backup. Datastore.AllocateSpace is
// the narrowest privilege that reveals them; Datastore.Allocate also works but
// additionally allows deleting volumes, which is exactly what a service
// account pointed at your backups must never be able to do.
//
// So "read-only" here means: cannot restore, start, stop or destroy anything,
// and cannot delete a backup. It can allocate space on a storage. Anyone who
// needs a strictly read-only path to their backup catalogue should use a
// Proxmox Backup Server, whose DatastoreAudit token really is read-only.
var ReadOnlyPrivileges = []string{
	"VM.Audit",
	"VM.Backup",
	"VM.GuestAgent.Audit",
	"Datastore.Audit",
	"Datastore.AllocateSpace",
	"Sys.Audit",
	"VM.Monitor",
}

// DrillPrivileges is granted to a RestoreLab drill service account: every
// ReadOnlyPrivileges entry, plus exactly what is required to allocate,
// configure, power and tear down *temporary* recovery-test workloads.
// Deliberately absent: anything touching production VMs, migration,
// replication, snapshots-as-backup, or cluster/user administration - the
// intent is that even a fully compromised drill token can only ever affect
// whatever it is scoped to via ACLs (see Bootstrap), never the wider
// cluster.
//   - VM.Allocate                 create and destroy VMs/CTs
//   - VM.Config.CPU               reconfigure CPU
//   - VM.Config.Disk              attach/detach/resize disks
//   - VM.Config.HWType            change guest hardware type (e.g. bios/machine)
//   - VM.Config.Memory            reconfigure memory
//   - VM.Config.Network           rewrite the network onto the isolated bridge
//   - VM.Config.Options           reconfigure general options (name, boot order...)
//   - VM.GuestAgent.Unrestricted  run validation commands inside the guest
//   - VM.PowerMgmt                start/stop a VM/CT
//   - Datastore.AllocateSpace     allocate space for a restored disk
var DrillPrivileges = append(append([]string{}, ReadOnlyPrivileges...), []string{
	"VM.Allocate",
	"VM.Config.CPU",
	"VM.Config.Disk",
	"VM.Config.HWType",
	"VM.Config.Memory",
	"VM.Config.Network",
	"VM.Config.Options",
	"VM.GuestAgent.Unrestricted",
	"VM.PowerMgmt",
	"Datastore.AllocateSpace",
}...)

// optionalPrivileges may legitimately not exist on a given Proxmox version.
// The privilege vocabulary is not stable across major releases: VM.Monitor
// was removed in Proxmox VE 9, and the VM.GuestAgent.* pair only appeared in
// 8. Rather than hardcoding one version's list and failing on every other,
// Bootstrap discovers what this cluster actually supports and drops the
// entries below when they are unknown, saying so.
//
// Anything NOT in this map is required: if the cluster does not know it,
// something is wrong enough to stop rather than silently create a role that
// cannot do its job.
var optionalPrivileges = map[string]string{
	"VM.Monitor":                 "runtime state reads, removed in Proxmox VE 9",
	"VM.GuestAgent.Audit":        "guest agent queries, used to discover the restored guest's address and OS",
	"VM.GuestAgent.Unrestricted": "in-guest command checks",
	"SDN.Use":                    "attaching a workload to the isolated bridge (Proxmox VE 9 and newer)",
}

// supportedPrivileges derives the privilege vocabulary of this cluster from
// the roles it already has. Every built-in role's privileges are, by
// definition, valid names on that version - and Administrator holds them all.
func supportedPrivileges(roles map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, privs := range roles {
		for _, p := range splitPrivs(privs) {
			out[p] = true
		}
	}
	return out
}

// filterPrivileges keeps what this cluster understands and reports the rest.
// An empty supported set means discovery failed, in which case nothing is
// dropped: guessing is worse than letting the API refuse.
func filterPrivileges(want []string, supported map[string]bool) (kept, dropped []string) {
	if len(supported) == 0 {
		return want, nil
	}
	for _, p := range want {
		if supported[p] {
			kept = append(kept, p)
			continue
		}
		dropped = append(dropped, p)
	}
	return kept, dropped
}

// describeDropped renders the "we left these out" note shown next to the role
// creation step, and errors when a privilege we cannot do without is missing.
func describeDropped(dropped []string) (string, error) {
	if len(dropped) == 0 {
		return "", nil
	}
	var required, optional []string
	for _, p := range dropped {
		if why, ok := optionalPrivileges[p]; ok {
			optional = append(optional, p+" - "+why)
			continue
		}
		required = append(required, p)
	}
	if len(required) > 0 {
		return "", fmt.Errorf("proxmox: this cluster does not know the privilege(s) %s, which RestoreLab cannot work without; please report your Proxmox VE version",
			strings.Join(required, ", "))
	}
	return "not supported on this Proxmox version, skipped: " + strings.Join(optional, "; "), nil
}

// storagePrivileges is granted, on a per-storage basis, to the auxiliary
// storage role Bootstrap creates for drill mode (see ensureStorageAccess).
// It is intentionally narrower than DrillPrivileges applied wholesale to
// /storage: a token should be able to audit and allocate on the storages it
// restores into, nothing more.
//
// It must carry EVERY privilege needed at that path, including Datastore.Audit
// which the broader /storage grant already provides. Proxmox ACLs do not
// accumulate down a path: an ACL on /storage/local replaces the one inherited
// from /storage rather than adding to it, so a narrower grant that omits a
// privilege silently takes it away.
var storagePrivileges = []string{"Datastore.Audit", "Datastore.AllocateSpace"}

// readOnlyRoleName is the fixed role name used for the cluster-wide,
// always-granted read-only ACLs (see Bootstrap's ACL step). In ReadOnly
// mode this is simply opts.RoleName (the caller's one role IS the read-only
// role). In drill mode opts.RoleName names a role with drill (write)
// privileges, which must never be the role backing a broad, cluster-wide
// grant - so drill mode ensures this second, always-read-only role in
// addition to opts.RoleName, and uses it for the /vms, /nodes and /storage
// baseline grants. Only the narrow /pool/{Pool} (or, with a loud warning,
// /vms) grant uses the drill role itself.
const readOnlyRoleName = "RestoreLabRead"

// ---------------------------------------------------------------------------
// Bootstrap options and result.
// ---------------------------------------------------------------------------

// BootstrapOptions configures one least-privilege provisioning run.
type BootstrapOptions struct {
	UserID    string // "restorelab@pve"
	Comment   string
	TokenName string   // "drills"
	RoleName  string   // "RestoreLabDrill" or "RestoreLabRead"
	Pool      string   // resource pool for temporary workloads; "" to skip pool creation
	ReadOnly  bool     // create a read-only role and read-only ACLs
	Node      string   // "" means every node
	Storages  []string // storages needing write access for restores; ignored when ReadOnly
	// Bridge is the isolated bridge drills restore onto. Proxmox VE 9 routes
	// every bridge access through the SDN permission tree, so using even a
	// plain Linux bridge requires SDN.Use on it. Empty skips the grant.
	Bridge string
	// SDNZone is the zone that bridge lives under. Classic, non-SDN bridges
	// sit in "localnetwork", which is the default.
	SDNZone string
	// ReuseExistingToken keeps a token that already exists instead of failing.
	// Proxmox reveals a secret only at creation, so the caller must already
	// hold it; this exists so re-running a bootstrap can reconcile roles and
	// ACLs without destroying a working token.
	ReuseExistingToken bool
	DryRun             bool // report what would be done, change nothing
}

// defaultSDNZone is where Proxmox files bridges that no SDN zone manages.
const defaultSDNZone = "localnetwork"

// sdnPrivileges lets the service account attach a workload to the isolated
// bridge. Granted only on that one bridge: it is the difference between "may
// use the recovery network" and "may attach a workload to any network on this
// cluster".
var sdnPrivileges = []string{"SDN.Use"}

func (o BootstrapOptions) validate() error {
	if o.UserID == "" {
		return errors.New("proxmox: BootstrapOptions.UserID is required")
	}
	if o.TokenName == "" {
		return errors.New("proxmox: BootstrapOptions.TokenName is required")
	}
	if o.RoleName == "" {
		return errors.New("proxmox: BootstrapOptions.RoleName is required")
	}
	return nil
}

// BootstrapResult is what Bootstrap produced. Secret is only ever populated
// once: PVE never reveals a token secret again after creation, and neither
// does RestoreLab (String redacts it; nothing in this package logs it).
type BootstrapResult struct {
	TokenID string // "restorelab@pve!drills"
	Secret  string // the token secret, returned exactly once by PVE
	Steps   []BootstrapStep
}

// String redacts Secret so BootstrapResult is safe to log or print.
func (r *BootstrapResult) String() string {
	return fmt.Sprintf("BootstrapResult{TokenID:%q, Secret:\"[REDACTED]\", Steps:%d}", r.TokenID, len(r.Steps))
}

// BootstrapStep records one provisioning action, in the order it was
// (or would have been) performed.
type BootstrapStep struct {
	Description string // "create role RestoreLabDrill"
	Status      string // "created" | "already exists" | "updated" | "skipped" | "would create"
	Detail      string
}

// ---------------------------------------------------------------------------
// Bootstrap.
// ---------------------------------------------------------------------------

// Bootstrap provisions (idempotently) a least-privilege RestoreLab service
// account, role(s), ACLs and API token on the connected PVE cluster. It
// requires a prior successful Login. Every step is recorded in
// BootstrapResult.Steps in the order performed, even when a later step
// fails - so a caller can show partial progress on error.
func (c *AdminClient) Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if !c.loggedIn() {
		return nil, errors.New("proxmox: Bootstrap requires a successful Login first")
	}

	result := &BootstrapResult{}
	record := func(step BootstrapStep) {
		if step.Description != "" {
			result.Steps = append(result.Steps, step)
		}
	}

	roles, err := c.listRoles(ctx)
	if err != nil {
		return result, fmt.Errorf("proxmox: list roles: %w", err)
	}
	// Captured once, from the cluster's own built-in roles, before anything
	// below starts adding roles of ours to the map.
	supported := supportedPrivileges(roles)

	// The role backing the broad, cluster-wide read-only grants below: the
	// caller's own role in ReadOnly mode, or the fixed readOnlyRoleName
	// alongside the caller's (more privileged) drill role otherwise.
	roRoleID := opts.RoleName
	if !opts.ReadOnly {
		roRoleID = readOnlyRoleName
	}
	roStep, err := c.ensureRole(ctx, roles, supported, roRoleID, ReadOnlyPrivileges, opts.DryRun)
	record(roStep)
	if err != nil {
		return result, err
	}
	roles[roRoleID] = privsKey(ReadOnlyPrivileges)

	drillRoleID := ""
	if !opts.ReadOnly {
		drillRoleID = opts.RoleName
		drillStep, err := c.ensureRole(ctx, roles, supported, drillRoleID, DrillPrivileges, opts.DryRun)
		record(drillStep)
		if err != nil {
			return result, err
		}
		roles[drillRoleID] = privsKey(DrillPrivileges)
	}

	if opts.Pool != "" && !opts.ReadOnly {
		pools, err := c.listPools(ctx)
		if err != nil {
			return result, fmt.Errorf("proxmox: list pools: %w", err)
		}
		poolStep, err := c.ensurePool(ctx, pools, opts.Pool, opts.Comment, opts.DryRun)
		record(poolStep)
		if err != nil {
			return result, err
		}
	}

	users, err := c.listUsers(ctx)
	if err != nil {
		return result, fmt.Errorf("proxmox: list users: %w", err)
	}
	userStep, err := c.ensureUser(ctx, users, opts.UserID, opts.Comment, opts.DryRun)
	record(userStep)
	if err != nil {
		return result, err
	}

	// Baseline read-only ACLs: always granted, regardless of mode, so the
	// token can at minimum see the cluster it operates against.
	nodesPath := "/nodes"
	if opts.Node != "" {
		nodesPath = "/nodes/" + opts.Node
	}
	for _, path := range []string{"/vms", nodesPath, "/storage"} {
		step, err := c.grantACL(ctx, path, opts.UserID, roRoleID, opts.DryRun)
		record(step)
		if err != nil {
			return result, err
		}
	}

	if !opts.ReadOnly {
		targetPath := "/vms"
		if opts.Pool != "" {
			targetPath = "/pool/" + opts.Pool
		}
		step, err := c.grantACL(ctx, targetPath, opts.UserID, drillRoleID, opts.DryRun)
		if opts.Pool == "" {
			// Loud, explicit warning: without a pool the drill role is
			// granted directly on /vms, so this token can create,
			// reconfigure and destroy ANY VM or container on the cluster -
			// not just the temporary workloads RestoreLab creates for
			// drills. Configure BootstrapOptions.Pool to scope it down.
			step.Detail = "no resource pool configured: this token's role is granted on /vms directly, so it can create, reconfigure and destroy ANY VM or container on the cluster, not only RestoreLab's own temporary drill workloads. Set BootstrapOptions.Pool to scope this token down to a resource pool."
		}
		record(step)
		if err != nil {
			return result, err
		}

		if len(opts.Storages) > 0 {
			storageRoleID := opts.RoleName + "Storage"
			storageStep, err := c.ensureRole(ctx, roles, supported, storageRoleID, storagePrivileges, opts.DryRun)
			record(storageStep)
			if err != nil {
				return result, err
			}
			roles[storageRoleID] = privsKey(storagePrivileges)

			for _, storage := range opts.Storages {
				step, err := c.grantACL(ctx, "/storage/"+storage, opts.UserID, storageRoleID, opts.DryRun)
				record(step)
				if err != nil {
					return result, err
				}
			}
		}
	}

	// The isolated bridge, scoped to itself. Proxmox VE 9 checks bridge access
	// through /sdn/zones/{zone}/{bridge}, so without this the restore is
	// refused with 403 SDN.Use even for a plain Linux bridge.
	if !opts.ReadOnly && opts.Bridge != "" {
		zone := opts.SDNZone
		if zone == "" {
			zone = defaultSDNZone
		}
		netRoleID := opts.RoleName + "Network"
		netStep, err := c.ensureRole(ctx, roles, supported, netRoleID, sdnPrivileges, opts.DryRun)
		record(netStep)
		if err != nil {
			return result, err
		}
		roles[netRoleID] = privsKey(sdnPrivileges)

		step, err := c.grantACL(ctx, fmt.Sprintf("/sdn/zones/%s/%s", zone, opts.Bridge), opts.UserID, netRoleID, opts.DryRun)
		record(step)
		if err != nil {
			return result, err
		}
	}

	tokenStep, tokenID, secret, err := c.ensureToken(ctx, opts.UserID, opts.TokenName, opts.ReuseExistingToken, opts.DryRun)
	record(tokenStep)
	if err != nil {
		return result, err
	}
	result.TokenID = tokenID
	result.Secret = secret
	return result, nil
}

// ---------------------------------------------------------------------------
// Idempotent ensure* helpers. Each performs the GET/POST/PUT pair described
// in the package-level Bootstrap doc comment and returns the BootstrapStep
// to record, so Bootstrap's own control flow stays linear and readable.
// ---------------------------------------------------------------------------

// privsKey normalises a privilege slice into PVE's comma-separated form,
// sorted so two privilege sets can be compared for equality regardless of
// the order they were declared or returned in.
func privsKey(privs []string) string {
	sorted := append([]string(nil), privs...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func splitPrivs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// decodeRows decodes a PVE list response, tolerating both an empty/absent
// body and a JSON `null` data field - both of which mean "no rows" on a
// freshly bootstrapped or empty cluster.
func decodeRows(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// listRoles returns existing roleid -> privs (comma-separated, as PVE
// stores it) via GET /access/roles.
func (c *AdminClient) listRoles(ctx context.Context) (map[string]string, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/access/roles", nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, fmt.Errorf("proxmox: decode roles: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[asString(row["roleid"])] = asString(row["privs"])
	}
	return out, nil
}

// ensureRole creates roleID with privs if absent, updates it if present
// with different privileges, or leaves it alone if it already matches.
// existing is consulted (and, by the caller, kept up to date) rather than
// re-fetched on every call so Bootstrap issues one GET /access/roles per
// run rather than one per role it needs to ensure.
func (c *AdminClient) ensureRole(ctx context.Context, existing map[string]string, supported map[string]bool, roleID string, privs []string, dryRun bool) (BootstrapStep, error) {
	desc := fmt.Sprintf("create role %s", roleID)

	// The privilege vocabulary differs between Proxmox major versions, so the
	// caller passes what this cluster actually knows, captured before any role
	// was created - deriving it from `existing` here would see RestoreLab's
	// own freshly created roles and narrow the vocabulary to itself.
	kept, dropped := filterPrivileges(privs, supported)
	note, err := describeDropped(dropped)
	if err != nil {
		return BootstrapStep{}, err
	}
	privs = kept
	desired := privsKey(privs)

	detail := func(base string) string {
		if note == "" {
			return base
		}
		return base + " - " + note
	}

	cur, exists := existing[roleID]
	switch {
	case exists && privsKey(splitPrivs(cur)) == desired:
		return BootstrapStep{Description: desc, Status: "already exists", Detail: detail("privileges: " + desired)}, nil

	case exists:
		if dryRun {
			return BootstrapStep{Description: desc, Status: "would create", Detail: detail("would update privileges to: " + desired)}, nil
		}
		if _, err := c.doRequest(ctx, http.MethodPut, "/access/roles/"+roleID, url.Values{
			"privs":  {desired},
			"append": {"0"},
		}); err != nil {
			return BootstrapStep{}, fmt.Errorf("proxmox: update role %s: %w", roleID, err)
		}
		return BootstrapStep{Description: desc, Status: "updated", Detail: detail("privileges: " + desired)}, nil

	default:
		if dryRun {
			return BootstrapStep{Description: desc, Status: "would create", Detail: detail("privileges: " + desired)}, nil
		}
		if _, err := c.doRequest(ctx, http.MethodPost, "/access/roles", url.Values{
			"roleid": {roleID},
			"privs":  {desired},
		}); err != nil {
			return BootstrapStep{}, fmt.Errorf("proxmox: create role %s: %w", roleID, err)
		}
		return BootstrapStep{Description: desc, Status: "created", Detail: detail("privileges: " + desired)}, nil
	}
}

// listPools returns the set of existing pool IDs via GET /pools.
func (c *AdminClient) listPools(ctx context.Context) (map[string]bool, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/pools", nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, fmt.Errorf("proxmox: decode pools: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[asString(row["poolid"])] = true
	}
	return out, nil
}

// ensurePool creates poolID if absent. PVE pools have no meaningful state
// to reconcile beyond existence, so unlike ensureRole there is no update
// branch.
func (c *AdminClient) ensurePool(ctx context.Context, existing map[string]bool, poolID, comment string, dryRun bool) (BootstrapStep, error) {
	desc := fmt.Sprintf("create pool %s", poolID)
	if existing[poolID] {
		return BootstrapStep{Description: desc, Status: "already exists"}, nil
	}
	if dryRun {
		return BootstrapStep{Description: desc, Status: "would create"}, nil
	}
	if _, err := c.doRequest(ctx, http.MethodPost, "/pools", url.Values{
		"poolid":  {poolID},
		"comment": {comment},
	}); err != nil {
		return BootstrapStep{}, fmt.Errorf("proxmox: create pool %s: %w", poolID, err)
	}
	return BootstrapStep{Description: desc, Status: "created"}, nil
}

// listUsers returns the set of existing user IDs via GET /access/users.
func (c *AdminClient) listUsers(ctx context.Context) (map[string]bool, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/access/users", nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, fmt.Errorf("proxmox: decode users: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[asString(row["userid"])] = true
	}
	return out, nil
}

// ensureUser creates userID (enabled) if absent.
func (c *AdminClient) ensureUser(ctx context.Context, existing map[string]bool, userID, comment string, dryRun bool) (BootstrapStep, error) {
	desc := fmt.Sprintf("create user %s", userID)
	if existing[userID] {
		return BootstrapStep{Description: desc, Status: "already exists"}, nil
	}
	if dryRun {
		return BootstrapStep{Description: desc, Status: "would create"}, nil
	}
	if _, err := c.doRequest(ctx, http.MethodPost, "/access/users", url.Values{
		"userid":  {userID},
		"comment": {comment},
		"enable":  {"1"},
	}); err != nil {
		return BootstrapStep{}, fmt.Errorf("proxmox: create user %s: %w", userID, err)
	}
	return BootstrapStep{Description: desc, Status: "created"}, nil
}

// grantACL grants roleID to userID on path with propagate=1. PVE's ACL PUT
// is itself an idempotent upsert (granting the same user+role on the same
// path twice is a no-op), so unlike roles/pools/users there is no
// existence pre-check here: it is always safe to issue, and only DryRun
// suppresses the write.
func (c *AdminClient) grantACL(ctx context.Context, path, userID, roleID string, dryRun bool) (BootstrapStep, error) {
	desc := fmt.Sprintf("grant %s on %s", roleID, path)
	if dryRun {
		return BootstrapStep{Description: desc, Status: "would create"}, nil
	}
	if _, err := c.doRequest(ctx, http.MethodPut, "/access/acl", url.Values{
		"path":      {path},
		"users":     {userID},
		"roles":     {roleID},
		"propagate": {"1"},
	}); err != nil {
		return BootstrapStep{}, fmt.Errorf("proxmox: grant %s on %s: %w", roleID, path, err)
	}
	return BootstrapStep{Description: desc, Status: "created"}, nil
}

// listTokenIDs returns the set of existing token names (without the
// "user@realm!" prefix) for userID via GET /access/users/{userid}/token.
func (c *AdminClient) listTokenIDs(ctx context.Context, userID string) (map[string]bool, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, "/access/users/"+userID+"/token", nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, fmt.Errorf("proxmox: decode tokens: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[asString(row["tokenid"])] = true
	}
	return out, nil
}

// ensureToken creates tokenName for userID with privsep=0 (so the token
// carries exactly userID's own ACL-granted privileges, nothing separate).
// PVE reveals a token's secret only at creation time and never again, so a
// pre-existing token name is a hard error: RestoreLab will not delete an
// existing token on the administrator's behalf (that would be a surprising,
// irreversible action to take implicitly), the caller must pick a new name
// or remove the old token itself.
func (c *AdminClient) ensureToken(ctx context.Context, userID, tokenName string, reuse, dryRun bool) (step BootstrapStep, tokenID, secret string, err error) {
	fullID := userID + "!" + tokenName
	desc := fmt.Sprintf("create token %s", fullID)

	existing, err := c.listTokenIDs(ctx, userID)
	if err != nil {
		return BootstrapStep{}, "", "", fmt.Errorf("proxmox: list tokens for %s: %w", userID, err)
	}
	if existing[tokenName] && reuse {
		// The caller says it already holds this secret. Reconciling roles and
		// ACLs must not require destroying a token that works.
		return BootstrapStep{
			Description: desc,
			Status:      "already exists",
			Detail:      "kept, along with the secret you already hold",
		}, fullID, "", nil
	}
	if existing[tokenName] {
		step := BootstrapStep{
			Description: desc,
			Status:      "skipped",
			Detail: fmt.Sprintf(
				"a token named %q already exists on %s; Proxmox reveals a token secret only once, at creation time, so RestoreLab cannot recover it and will not delete the existing token automatically - choose a different TokenName or delete the existing token yourself first",
				tokenName, userID,
			),
		}
		return step, "", "", fmt.Errorf("proxmox: token %q already exists for user %s and its secret cannot be recovered; choose a new TokenName or delete the token first", tokenName, userID)
	}

	if dryRun {
		return BootstrapStep{Description: desc, Status: "would create"}, fullID, "", nil
	}

	raw, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("/access/users/%s/token/%s", userID, tokenName), url.Values{
		"privsep": {"0"},
	})
	if err != nil {
		return BootstrapStep{}, "", "", fmt.Errorf("proxmox: create token %s: %w", fullID, err)
	}

	var data struct {
		Value       string `json:"value"`
		FullTokenID string `json:"full-tokenid"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return BootstrapStep{}, "", "", fmt.Errorf("proxmox: decode token response: %w", err)
	}
	if data.Value == "" || data.FullTokenID == "" {
		return BootstrapStep{}, "", "", errors.New("proxmox: token creation response missing value or full-tokenid")
	}

	return BootstrapStep{Description: desc, Status: "created"}, data.FullTokenID, data.Value, nil
}
