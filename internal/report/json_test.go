package report

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestJSON_SchemaAndShape(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, fixtureRunFailed()); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if raw["schema"] != SchemaVersion {
		t.Errorf("schema = %v, want %v", raw["schema"], SchemaVersion)
	}

	// Durations must appear in both the "_seconds" float form and the human
	// string form.
	if _, ok := raw["rto_seconds"].(float64); !ok {
		t.Errorf("rto_seconds missing or not a number: %v", raw["rto_seconds"])
	}
	if _, ok := raw["rto"].(string); !ok {
		t.Errorf("rto missing or not a string: %v", raw["rto"])
	}

	steps, ok := raw["steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("expected a non-empty steps array, got %v", raw["steps"])
	}
	step0, ok := steps[0].(map[string]any)
	if !ok {
		t.Fatalf("step 0 is not an object: %v", steps[0])
	}
	if _, ok := step0["duration_seconds"].(float64); !ok {
		t.Errorf("step.duration_seconds missing or not a number: %v", step0["duration_seconds"])
	}
	if _, ok := step0["duration"].(string); !ok {
		t.Errorf("step.duration missing or not a string: %v", step0["duration"])
	}

	checks, ok := raw["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatalf("expected a non-empty checks array, got %v", raw["checks"])
	}
	check0, ok := checks[0].(map[string]any)
	if !ok {
		t.Fatalf("check 0 is not an object: %v", checks[0])
	}
	for _, field := range []string{"name", "type", "status", "duration_seconds", "duration", "pass"} {
		if _, ok := check0[field]; !ok {
			t.Errorf("check missing field %q: %v", field, check0)
		}
	}

	if raw["backup"] == nil {
		t.Errorf("expected non-nil backup metadata for a run with a backup")
	}

	// Also confirm the typed DTO round-trips cleanly.
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal into Document: %v", err)
	}
	if doc.Schema != SchemaVersion {
		t.Errorf("Document.Schema = %q, want %q", doc.Schema, SchemaVersion)
	}
	if doc.Result != "FAILED" {
		t.Errorf("Document.Result = %q, want FAILED", doc.Result)
	}
	if len(doc.Checks) == 0 {
		t.Fatalf("expected checks in decoded Document")
	}
	foundFailure := false
	for _, c := range doc.Checks {
		if c.Message == httpHealthFailMessage {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Errorf("expected the failing check's message %q among decoded checks", httpHealthFailMessage)
	}
}

func TestJSON_NilBackupDoesNotPanic(t *testing.T) {
	run := fixtureRunSuccess()
	run.Backup = nil

	var buf bytes.Buffer
	if err := JSON(&buf, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Backup != nil {
		t.Errorf("expected nil Backup, got %+v", doc.Backup)
	}
}

func TestJSON_RTOTargetOmittedWhenUnset(t *testing.T) {
	run := fixtureRunSuccess()
	run.RTOTarget = 0

	var buf bytes.Buffer
	if err := JSON(&buf, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, ok := raw["rto_target"]; ok {
		t.Errorf("expected rto_target to be omitted when RTOTarget is zero, got %v", raw["rto_target"])
	}
}

func TestNewDocumentIsTheDocumentJSONWrites(t *testing.T) {
	run := fixtureRunFailed()

	doc := NewDocument(run)
	if doc.Schema != SchemaVersion {
		t.Errorf("Schema = %q, want %q", doc.Schema, SchemaVersion)
	}
	if doc.RunID != run.ID {
		t.Errorf("RunID = %q, want %q", doc.RunID, run.ID)
	}

	// Ce que NewDocument construit et ce que JSON écrit doivent être le même
	// document : deux chemins qui divergeraient, c'est un rapport qui ment
	// selon la porte par laquelle on entre.
	var buf bytes.Buffer
	if err := JSON(&buf, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	direct, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fromWriter, fromDoc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &fromWriter); err != nil {
		t.Fatalf("unmarshal writer output: %v", err)
	}
	if err := json.Unmarshal(direct, &fromDoc); err != nil {
		t.Fatalf("unmarshal document: %v", err)
	}
	if !reflect.DeepEqual(fromWriter, fromDoc) {
		t.Error("JSON() and NewDocument() produced different documents")
	}
}

func TestNewBackupDTOOnNilIsNil(t *testing.T) {
	if got := NewBackupDTO(nil); got != nil {
		t.Fatalf("NewBackupDTO(nil) = %+v, want nil", got)
	}
}

// A report says which stored plan produced the run, and which version of it.
// "postgres-nightly" alone cannot answer "which version", and the plan may
// well have been edited since the drill ran.
func TestJSON_CarriesTheStoredPlanItCameFrom(t *testing.T) {
	run := fixtureRunSuccess()
	run.PlanID = "2f1a4c76-0b1e-4d2a-9a51-1d0f8c2b3e44"
	run.PlanVersion = 2

	var buf bytes.Buffer
	if err := JSON(&buf, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if raw["plan_id"] != run.PlanID {
		t.Errorf("plan_id = %v, want %q", raw["plan_id"], run.PlanID)
	}
	if raw["plan_version"] != float64(2) {
		t.Errorf("plan_version = %v, want 2", raw["plan_version"])
	}
}

// An ad-hoc drill has no plan behind it, and a report that showed an empty
// id would invite a reader to go looking for a plan that never existed.
func TestJSON_ProvenanceIsOmittedForAnAdhocDrill(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, fixtureRunSuccess()); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, field := range []string{"plan_id", "plan_version"} {
		if _, ok := raw[field]; ok {
			t.Errorf("%s is present on an ad-hoc run: %v", field, raw[field])
		}
	}
}

// A report of a finished drill must read the same every time it is rendered.
//
// The backup's age used to be time.Since(CreatedAt), recomputed at render
// time, so the same archived run reported a slightly older backup on every
// read - and two renders taken microseconds apart were not byte-identical.
// That is what broke TestNewDocumentIsTheDocumentJSONWrites the first time CI
// ran it on Linux: Windows' coarse clock had been returning the same instant
// to both calls and hiding it.
//
// The age that belongs in a drill's report is the age at the moment the drill
// used the backup, which is a fact about the drill and does not move.
func TestBackupAgeIsMeasuredAtTheStartOfTheRun(t *testing.T) {
	run := fixtureRunSuccess()
	doc := NewDocument(run)

	want := run.StartedAt.Sub(run.Backup.CreatedAt).Seconds()
	if doc.Backup.AgeSeconds != want {
		t.Errorf("age_seconds = %v, want %v (the backup's age when the drill restored it)",
			doc.Backup.AgeSeconds, want)
	}
	if doc.Backup.Age != FormatDuration(run.StartedAt.Sub(run.Backup.CreatedAt)) {
		t.Errorf("age = %q, want %q", doc.Backup.Age,
			FormatDuration(run.StartedAt.Sub(run.Backup.CreatedAt)))
	}
}

func TestRenderingTheSameRunTwiceGivesTheSameBytes(t *testing.T) {
	run := fixtureRunSuccess()

	var first, second bytes.Buffer
	if err := JSON(&first, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if err := JSON(&second, run); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if first.String() != second.String() {
		t.Error("two renders of one finished run differ: an archived report must not move")
	}
}
