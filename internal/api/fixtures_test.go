package api

// The golden fixtures: the real HTTP body of every route the dashboard reads,
// captured into web/src/api/__fixtures__ and committed.
//
// They exist because web/src/api/types.ts is a hand-written mirror of these
// DTOs and nothing else checks that the two agree. Capturing the response
// rather than marshalling a DTO is the point: what drifts is the page
// envelope, the interplay of omitempty, and the shape of a problem+json - not
// the struct taken on its own.
//
// Regenerate with:
//
//	go test ./internal/api/ -run TestFixturesMatchTheWire -update

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

var updateFixtures = flag.Bool("update", false,
	"rewrite the captured API fixtures under web/src/api/__fixtures__")

// fixtureDir is where the TypeScript side reads them from. The path is
// relative to this package, which is where `go test` runs.
const fixtureDir = "../../web/src/api/__fixtures__"

// fixtureNow is the instant every capture is taken at. A fixture carrying a
// real clock would differ on every run and the golden comparison could not
// hold.
var fixtureNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// The identifiers the captures carry. They are written out rather than
// generated for the same reason the clock is fixed: a uuid minted per run
// would land in a committed file and make every regeneration a diff.
const (
	fixtureFinishedRunID  = "8f1c6d20-0000-4000-8000-000000000001"
	fixtureRunningRunID   = "8f1c6d20-0000-4000-8000-000000000002"
	fixtureQueuedRunID    = "8f1c6d20-0000-4000-8000-000000000003"
	fixtureTriggeredRunID = "8f1c6d20-0000-4000-8000-00000000000f"
	fixturePlanID         = "1f0b2a44-0000-4000-8000-00000000000a"
)

// fixtureCase is one capture: a name, and a body to compare or write.
type fixtureCase struct {
	name string
	body func(t *testing.T) []byte
}

// recorded returns a recorder's body, failing the test when the status is not
// the one the fixture is meant to show.
func recorded(t *testing.T, rec *httptest.ResponseRecorder, want int) []byte {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
	}
	return rec.Body.Bytes()
}

// fixtureBackup is the backup the drills in these captures restored from.
// Its creation time is fixed rather than relative, so that it agrees with the
// timestamp its provider id spells out; a report renders its age against the
// run's start, never against now, so the document does not move between two
// reads.
// fixturePlanYAML is a plan a human wrote: a leading comment, quoted ids, and
// defaults left out. The comment is load-bearing - the catalogue stores a
// document verbatim, and a capture without one would let an editor be written
// as though comments did not survive a round trip.
const fixturePlanYAML = `# the web tier, restored nightly
name: web-tier
description: nightly drill of the web tier
workload:
  provider: proxmox-main
  id: "110"
backup:
  strategy: latest
checks:
  - type: tcp
    port: 22
cleanup:
  always: true
rto_target: 5m
`

// fixturePlan is the catalogue row those bytes make, with an id that does not
// move between runs.
//
// It is seeded into the fake rather than created through POST: catalog.Save
// generates a UUID, and a capture carrying one would differ on every run. The
// alternative - making that generator injectable too - would be production
// code changed for a capture that does not need it.
func fixturePlan() store.Plan {
	return store.Plan{
		ID:          "1f0b2a44-0000-4000-8000-00000000000a",
		Name:        "web-tier",
		Description: "nightly drill of the web tier",
		WorkloadID:  "110",
		ProviderID:  "proxmox-main",
		YAML:        fixturePlanYAML,
		Version:     2,
		CreatedAt:   fixtureNow.Add(-72 * time.Hour),
		UpdatedAt:   fixtureNow.Add(-2 * time.Hour),
	}
}

func fixtureBackup() *core.Backup {
	return &core.Backup{
		ID:         "local:backup/vzdump-qemu-110-2026_09_01-07_00_00.vma.zst",
		WorkloadID: "110",
		ProviderID: "pve",
		Datastore:  "local",
		Node:       "pve1",
		CreatedAt:  time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC),
		SizeBytes:  337 << 20,
		Verified:   core.VerificationNone,
		Format:     "vma.zst",
	}
}

// fixtureFinishedRun is a drill that ran to the end, with steps, checks, a
// backup and an RTO measured against its target.
func fixtureFinishedRun() core.RecoveryRun {
	started := fixtureNow.Add(-2 * time.Hour)
	return core.RecoveryRun{
		ID:          fixtureFinishedRunID,
		PlanName:    "web-tier",
		PlanID:      fixturePlanID,
		PlanVersion: 3,

		ProviderID:       "pve",
		BackupProviderID: "pve",

		SourceWorkloadID: "110",
		SourceName:       "linux-test",
		TempWorkloadID:   "9001",
		TempName:         "restorelab-110",
		Node:             "pve1",

		Backup: fixtureBackup(),

		State:  core.RunSuccess,
		Result: core.ResultSuccess,

		StartedAt:   started,
		CompletedAt: started.Add(96 * time.Second),

		Steps: []core.Step{
			{
				Name: "discover-backup", State: core.RunDiscoveringBackup, Status: core.StepDone,
				StartedAt: started, CompletedAt: started.Add(2 * time.Second),
				Duration: 2 * time.Second,
				Message:  "latest backup is 3h00m00s old",
			},
			{
				Name: "restore", State: core.RunRestoring, Status: core.StepDone,
				StartedAt: started.Add(2 * time.Second), CompletedAt: started.Add(62 * time.Second),
				Duration: 60 * time.Second,
				Message:  "restored into workload 9001 on pve1",
			},
			{
				Name: "wait-for-guest", State: core.RunWaitingForGuest, Status: core.StepDone,
				StartedAt: started.Add(62 * time.Second), CompletedAt: started.Add(84 * time.Second),
				Duration: 22 * time.Second,
			},
			{
				Name: "cleanup", State: core.RunCleaningUp, Status: core.StepDone,
				StartedAt: started.Add(90 * time.Second), CompletedAt: started.Add(96 * time.Second),
				Duration: 6 * time.Second,
			},
		},

		Checks: []core.CheckResult{
			{
				Name: "ssh", Type: "tcp", Status: core.CheckPass,
				StartedAt: started.Add(84 * time.Second), CompletedAt: started.Add(85 * time.Second),
				Duration: 1200 * time.Millisecond, Attempts: 2,
				Message: "10.10.10.5:22 accepted a connection",
			},
			{
				Name: "homepage", Type: "http", Status: core.CheckPass,
				StartedAt: started.Add(85 * time.Second), CompletedAt: started.Add(86 * time.Second),
				Duration: 340 * time.Millisecond, Attempts: 1,
				Message: "GET / returned 200",
				Details: map[string]any{"status_code": 200, "url": "http://10.10.10.5/"},
			},
		},

		RTO:       84 * time.Second,
		RTOTarget: 5 * time.Minute,

		CleanupDone: true,
	}
}

// fixtureRunningRun is a drill a worker is executing right now.
//
// It carries a temporary workload id and a node on purpose: those two fields
// are what a cancellation dialogue has to name - "this will destroy 9001 on
// pve1" - and a capture without them would let that screen be written against
// nothing.
func fixtureRunningRun() core.RecoveryRun {
	started := fixtureNow.Add(-3 * time.Minute)
	return core.RecoveryRun{
		ID:       fixtureRunningRunID,
		PlanName: "adhoc-110",

		ProviderID:       "pve",
		BackupProviderID: "pve",

		SourceWorkloadID: "110",
		SourceName:       "linux-test",
		TempWorkloadID:   "9001",
		TempName:         "restorelab-110",
		Node:             "pve1",

		Backup: fixtureBackup(),

		State: core.RunWaitingForGuest,

		StartedAt: started,

		Steps: []core.Step{
			{
				Name: "discover-backup", State: core.RunDiscoveringBackup, Status: core.StepDone,
				StartedAt: started, CompletedAt: started.Add(2 * time.Second),
				Duration: 2 * time.Second,
			},
			{
				Name: "restore", State: core.RunRestoring, Status: core.StepDone,
				StartedAt: started.Add(2 * time.Second), CompletedAt: started.Add(62 * time.Second),
				Duration: 60 * time.Second,
			},
			{
				Name: "wait-for-guest", State: core.RunWaitingForGuest, Status: core.StepRunning,
				StartedAt: started.Add(62 * time.Second),
				Message:   "waiting for the guest agent to answer",
			},
		},

		RTOTarget: 5 * time.Minute,
	}
}

// fixtureQueuedRun is a drill nothing has picked up yet.
func fixtureQueuedRun() core.RecoveryRun {
	return core.RecoveryRun{
		ID:               fixtureQueuedRunID,
		PlanName:         "db-tier",
		PlanID:           fixturePlanID,
		PlanVersion:      3,
		ProviderID:       "pve",
		SourceWorkloadID: "120",
		SourceName:       "db-node",
		State:            core.RunQueued,
		StartedAt:        fixtureNow.Add(-1 * time.Minute),
		RTOTarget:        10 * time.Minute,
	}
}

// fixtureHistory is the drill history every run capture reads: one finished
// drill, one a worker holds, one still waiting. Ordered newest first by add,
// exactly as the store orders a listing.
func fixtureHistory() *fakeHistory {
	history := newFakeHistory()
	history.add(fixtureFinishedRun())
	history.add(fixtureRunningRun())
	history.add(fixtureQueuedRun())
	history.leases[fixtureRunningRunID] = fakeLease{
		owner:   "worker-a",
		expires: fixtureNow.Add(90 * time.Second),
	}
	history.events[fixtureFinishedRunID] = fixtureEvents()
	return history
}

// fixtureEvents is the progress stream of the finished drill, including one
// line carrying a check result: the check is a nested object the TypeScript
// side mirrors separately, and an event page without one would not show it.
func fixtureEvents() []store.Event {
	started := fixtureNow.Add(-2 * time.Hour)
	return []store.Event{
		{
			Seq: 1, At: started, State: core.RunQueued,
			Message: "drill queued",
		},
		{
			Seq: 2, At: started.Add(2 * time.Second), State: core.RunRestoring,
			Step: "restore", Status: core.StepRunning,
			Message: "restoring local:backup/vzdump-qemu-110-2026_09_01-07_00_00.vma.zst into 9001",
		},
		{
			Seq: 3, At: started.Add(85 * time.Second), State: core.RunRunningChecks,
			Step: "checks", Status: core.StepDone,
			Check: &core.CheckResult{
				Name: "ssh", Type: "tcp", Status: core.CheckPass,
				StartedAt: started.Add(84 * time.Second), CompletedAt: started.Add(85 * time.Second),
				Duration: 1200 * time.Millisecond, Attempts: 2,
				Message: "10.10.10.5:22 accepted a connection",
			},
		},
		{
			Seq: 4, At: started.Add(96 * time.Second), State: core.RunSuccess,
			Step: "cleanup", Status: core.StepDone,
			Message: "temporary workload 9001 destroyed",
		},
	}
}

// fixtureReadServer wires a read-only server over a history, at the fixed
// clock. Every capture that only reads goes through it.
func fixtureReadServer(t *testing.T, history History, providers ProviderSet) *Server {
	t.Helper()
	s, _ := newTestServer(t, Options{
		History:   history,
		Providers: providers,
		Now:       func() time.Time { return fixtureNow },
	})
	return s
}

func fixtureCases() []fixtureCase {
	return []fixtureCase{
		{"session", func(t *testing.T) []byte {
			f := newSessionFixture(t, store.ScopeOperate)
			resp := f.login(t)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("login status = %d, want 200", resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read login body: %v", err)
			}
			return body
		}},

		{"runs-page", func(t *testing.T) []byte {
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			return recorded(t, do(s, http.MethodGet, "/api/v1/recovery-runs"), http.StatusOK)
		}},

		{"run-finished", func(t *testing.T) []byte {
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			return recorded(t, do(s, http.MethodGet,
				"/api/v1/recovery-runs/"+fixtureFinishedRunID), http.StatusOK)
		}},

		{"run-running", func(t *testing.T) []byte {
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			return recorded(t, do(s, http.MethodGet,
				"/api/v1/recovery-runs/"+fixtureRunningRunID), http.StatusOK)
		}},

		{"run-events", func(t *testing.T) []byte {
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			// No Accept: text/event-stream, so this is the JSON page a script
			// polls, not the stream a dashboard follows. The stream carries
			// the same event objects inside SSE frames, which are not JSON.
			return recorded(t, do(s, http.MethodGet,
				"/api/v1/recovery-runs/"+fixtureFinishedRunID+"/events"), http.StatusOK)
		}},

		{"queue", func(t *testing.T) []byte {
			// The history holds one drill a worker leases and one still
			// waiting: the entry with a worker and the entry without are two
			// different shapes, and the queue screen renders both.
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			return recorded(t, do(s, http.MethodGet, "/api/v1/queue"), http.StatusOK)
		}},

		{"workloads-page", func(t *testing.T) []byte {
			history := newFakeHistory()
			history.lastRuns = map[string]store.RunSummary{
				"110": {
					ID:               fixtureFinishedRunID,
					SourceWorkloadID: "110",
					SourceName:       "linux-test",
					State:            core.RunSuccess,
					Result:           core.ResultSuccess,
					StartedAt:        fixtureNow.Add(-2 * time.Hour),
				},
			}
			s, _ := newTestServer(t, Options{
				History:   history,
				Providers: fakeProviders{hv: testFleet(t)},
				Now:       func() time.Time { return fixtureNow },
			})
			// temporary=true so the capture carries a managed workload: the
			// orphan banner is built against this fixture, and a page that
			// never shows one would let it be written against nothing.
			return recorded(t, do(s, http.MethodGet, "/api/v1/workloads?temporary=true"), http.StatusOK)
		}},

		{"workload", func(t *testing.T) []byte {
			history := newFakeHistory()
			history.lastRuns = map[string]store.RunSummary{
				"110": {
					ID:               fixtureFinishedRunID,
					SourceWorkloadID: "110",
					SourceName:       "linux-test",
					State:            core.RunSuccess,
					Result:           core.ResultSuccess,
					StartedAt:        fixtureNow.Add(-2 * time.Hour),
				},
			}
			s := fixtureReadServer(t, history, fakeProviders{hv: testFleet(t)})
			return recorded(t, do(s, http.MethodGet, "/api/v1/workloads/110"), http.StatusOK)
		}},

		{"backups", func(t *testing.T) []byte {
			// The age of a backup holds still here because the handler ages
			// it against the server's injected clock. It did not always: this
			// capture is what found that it was reading time.Now() instead.
			backups := []core.Backup{*fixtureBackup()}
			s := fixtureReadServer(t, newFakeHistory(),
				fakeProviders{hv: testFleet(t), bp: backupSource{backups: backups}})
			return recorded(t, do(s, http.MethodGet, "/api/v1/workloads/110/backups"), http.StatusOK)
		}},

		{"confidence", func(t *testing.T) []byte {
			history := newFakeHistory()
			history.add(fixtureFinishedRun())
			backups := []core.Backup{*fixtureBackup()}
			s := fixtureReadServer(t, history,
				fakeProviders{hv: testFleet(t), bp: backupSource{backups: backups}})
			return recorded(t, do(s, http.MethodGet, "/api/v1/workloads/110/confidence"), http.StatusOK)
		}},

		{"doctor", func(t *testing.T) []byte {
			cfg := testConfig()
			s, _ := newTestServer(t, Options{
				Config:    cfg,
				Providers: fakeProviders{hv: testFleet(t), bp: backupSource{}, entries: cfg.Providers},
				Now:       func() time.Time { return fixtureNow },
			})
			return recorded(t, do(s, http.MethodGet, "/api/v1/doctor"), http.StatusOK)
		}},

		{"providers", func(t *testing.T) []byte {
			cfg := testConfig()
			s, _ := newTestServer(t, Options{
				Config:    cfg,
				Providers: fakeProviders{hv: testFleet(t), bp: backupSource{}, entries: cfg.Providers},
				Now:       func() time.Time { return fixtureNow },
			})
			return recorded(t, do(s, http.MethodGet, "/api/v1/providers"), http.StatusOK)
		}},

		{"trigger-201", func(t *testing.T) []byte {
			s := operatingServer(t, newFakeHistory(),
				fakeProviders{hv: testFleet(t), bp: backupSource{}})
			// The id a trigger mints is the one value in this body that is
			// generated rather than read back, so it is injected.
			s.newID = func() string { return fixtureTriggeredRunID }
			return recorded(t, post(s, operateSecret, "/api/v1/recovery-runs",
				`{"workload_id":"110"}`), http.StatusCreated)
		}},

		{"cancel-200", func(t *testing.T) []byte {
			// Still queued: the store settles it here and now, and the body
			// is the run already CANCELLED.
			history := newFakeHistory()
			history.add(fixtureQueuedRun())
			s := operatingServer(t, history, nil)
			return recorded(t, post(s, operateSecret,
				"/api/v1/recovery-runs/"+fixtureQueuedRunID+"/cancel", ""), http.StatusOK)
		}},

		{"cancel-202", func(t *testing.T) []byte {
			// A worker is executing it: the API can only ask, and the body is
			// the run still in flight. A UI that rendered this the same way
			// as the 200 would report a machine gone that still exists.
			history := newFakeHistory()
			history.add(fixtureRunningRun())
			history.leases[fixtureRunningRunID] = fakeLease{
				owner:   "worker-a",
				expires: fixtureNow.Add(90 * time.Second),
			}
			s := operatingServer(t, history, nil)
			return recorded(t, post(s, operateSecret,
				"/api/v1/recovery-runs/"+fixtureRunningRunID+"/cancel", ""), http.StatusAccepted)
		}},

		{"cleanup", func(t *testing.T) []byte {
			s := operatingServer(t, newFakeHistory(),
				fakeProviders{hv: &cleanableFleet{fleet: testFleet(t)}})
			return recorded(t, post(s, operateSecret, "/api/v1/cleanup/9001", ""), http.StatusOK)
		}},

		{"plans-page", func(t *testing.T) []byte {
			s, plans := planServer(t)
			p := fixturePlan()
			plans.stored[p.ID] = p
			// A listing ships no documents: fifty plans must not become fifty
			// YAML blobs to draw a table. The absence of "yaml" in this
			// capture is the contract, and capturing it is how it stays one.
			return recorded(t, do(s, http.MethodGet, "/api/v1/plans"), http.StatusOK)
		}},

		{"plan", func(t *testing.T) []byte {
			s, plans := planServer(t)
			p := fixturePlan()
			plans.stored[p.ID] = p
			return recorded(t, do(s, http.MethodGet, "/api/v1/plans/web-tier"), http.StatusOK)
		}},

		{"validate-ok", func(t *testing.T) []byte {
			s, _ := planServer(t)
			// normalized_yaml comes back longer than what went in: that is the
			// defaulting, and showing it is the editor panel's whole point.
			return recorded(t, send(s, http.MethodPost, manageSecret,
				"/api/v1/plans/validate", fixturePlanYAML), http.StatusOK)
		}},

		{"validate-invalid", func(t *testing.T) []byte {
			s, _ := planServer(t)
			// A document that is valid YAML and is not a plan: no workload id.
			// The editor's job is to render this refusal, so it must render
			// the one the Go side actually words rather than a generic
			// "invalid" invented in TypeScript.
			return recorded(t, send(s, http.MethodPost, manageSecret,
				"/api/v1/plans/validate", "name: broken\nworkload:\n  provider: proxmox-main\n"),
				http.StatusBadRequest)
		}},

		{"problem-409-version", func(t *testing.T) []byte {
			s, plans := planServer(t)
			p := fixturePlan()
			plans.stored[p.ID] = p
			// Version 2 is stored; this PUT still believes in 1. It is the
			// conflict the editor must tell apart from a rename, which is the
			// other 409 this route answers.
			return recorded(t, send(s, http.MethodPut, manageSecret,
				"/api/v1/plans/web-tier?version=1", fixturePlanYAML),
				http.StatusConflict)
		}},

		{"setup-result", func(t *testing.T) []byte {
			s := setupServer(t, &fakeSetup{result: okResult()})
			return recorded(t, postSetup(s, setupSecret, validSetupBody), http.StatusOK)
		}},

		{"setup-failed", func(t *testing.T) []byte {
			// The refusal carries the steps already performed: that is what
			// lets the wizard show partial progress instead of a dead end,
			// and every one of those steps is idempotent so running it again
			// is safe.
			s := setupServer(t, &fakeSetup{
				result: &SetupResult{Steps: []SetupStep{
					{Description: "create role RestoreLabDrill", Status: "created"},
					{Description: "create user restorelab@pve", Status: "failed"},
				}},
				err: errors.New("proxmox: create user: 403 Permission check failed"),
			})
			return recorded(t, postSetup(s, setupSecret, validSetupBody), http.StatusBadGateway)
		}},

		{"problem-401", func(t *testing.T) []byte {
			s, _ := newTestServer(t, Options{})
			r := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, r)
			return recorded(t, rec, http.StatusUnauthorized)
		}},

		{"problem-404", func(t *testing.T) []byte {
			s := fixtureReadServer(t, fixtureHistory(), fakeProviders{})
			return recorded(t, do(s, http.MethodGet, "/api/v1/recovery-runs/nope"),
				http.StatusNotFound)
		}},

		{"problem-409", func(t *testing.T) []byte {
			// One drill per workload at a time: the run already in flight is
			// named in the detail, which is the only thing that tells whoever
			// got this which drill to go and look at.
			history := newFakeHistory()
			history.add(fixtureRunningRun())
			s := operatingServer(t, history,
				fakeProviders{hv: testFleet(t), bp: backupSource{}})
			return recorded(t, post(s, operateSecret, "/api/v1/recovery-runs",
				`{"workload_id":"110"}`), http.StatusConflict)
		}},
	}
}

// TestFixturesMatchTheWire fails when a response has changed shape without
// its fixture being regenerated - which is the whole point: the failure is
// the notice that the TypeScript mirror is now behind.
func TestFixturesMatchTheWire(t *testing.T) {
	for _, c := range fixtureCases() {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(fixtureDir, c.name+".json")

			// Re-indented rather than written raw: a fixture a human has to
			// read in a diff is worth two spaces, and the indentation is
			// stable so it never shows up as noise.
			var indented bytes.Buffer
			if err := json.Indent(&indented, c.body(t), "", "  "); err != nil {
				t.Fatalf("the response is not JSON: %v", err)
			}
			indented.WriteByte('\n')
			got := indented.Bytes()

			if *updateFixtures {
				if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
					t.Fatalf("create %s: %v", fixtureDir, err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s is missing: regenerate with\n"+
					"\tgo test ./internal/api/ -run TestFixturesMatchTheWire -update", path)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s is out of date.\n\ngot:\n%s\nwant:\n%s\n\n"+
					"If the API changed on purpose, regenerate with\n"+
					"\tgo test ./internal/api/ -run TestFixturesMatchTheWire -update\n"+
					"and then check web/src/api/types.ts still describes it.",
					path, got, want)
			}
		})
	}
}
