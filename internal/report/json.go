package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// SchemaVersion is the current value of Document.Schema. Bump it (and add a
// new schema constant alongside it, never repurpose this one) whenever a
// field is renamed or removed. Adding an optional field is not a breaking
// change and does not require a bump.
const SchemaVersion = "restorelab.recovery-run/v1"

// Document is the root of the JSON recovery-run report, schema
// "restorelab.recovery-run/v1".
//
// This is a deliberately stable, hand-maintained wire format aimed at CI
// pipelines and the future RestoreLab API: it never marshals core.RecoveryRun
// or any other domain type directly, so a refactor of the internal domain
// model does not silently change the document consumers parse. Every
// duration is emitted twice: once as "<name>_seconds" (a float64, easy to
// compare/aggregate) and once as "<name>" (a compact human string, e.g.
// "1m24s") for display. Timestamps are RFC 3339 in UTC. Fields that are
// naturally absent (no backup, no temporary workload, no error) are either
// null or omitted rather than rendered as zero values, so consumers can
// distinguish "zero" from "not applicable".
//
// Compatibility contract: existing field names and types are not changed or
// removed without a new schema version. New optional fields may be added at
// any time; consumers should ignore fields they do not recognise.
type Document struct {
	Schema string `json:"schema"`

	RunID    string `json:"run_id"`
	PlanName string `json:"plan_name"`

	ProviderID       string `json:"provider_id,omitempty"`
	BackupProviderID string `json:"backup_provider_id,omitempty"`

	SourceWorkloadID string `json:"source_workload_id"`
	SourceName       string `json:"source_name"`
	TempWorkloadID   string `json:"temp_workload_id,omitempty"`
	TempName         string `json:"temp_name,omitempty"`
	Node             string `json:"node,omitempty"`

	Backup *BackupDTO `json:"backup"`

	State  string `json:"state"`
	Result string `json:"result"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`

	Steps  []StepDTO  `json:"steps"`
	Checks []CheckDTO `json:"checks"`

	RTOSeconds float64 `json:"rto_seconds"`
	RTO        string  `json:"rto"`

	RTOTargetSeconds *float64 `json:"rto_target_seconds,omitempty"`
	RTOTarget        *string  `json:"rto_target,omitempty"`
	RTOExceeded      bool     `json:"rto_exceeded"`

	CleanupDone bool   `json:"cleanup_done"`
	Error       string `json:"error,omitempty"`
}

// BackupDTO describes the backup a run restored from.
type BackupDTO struct {
	ID         string `json:"id"`
	WorkloadID string `json:"workload_id"`
	ProviderID string `json:"provider_id,omitempty"`
	Datastore  string `json:"datastore,omitempty"`
	Node       string `json:"node,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	AgeSeconds float64   `json:"age_seconds"`
	Age        string    `json:"age"`

	SizeBytes int64  `json:"size_bytes"`
	Size      string `json:"size"`

	Protected bool   `json:"protected"`
	Encrypted bool   `json:"encrypted"`
	Verified  string `json:"verified"`
	Format    string `json:"format,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

// StepDTO is one executed stage of the recovery workflow.
type StepDTO struct {
	Name  string `json:"name"`
	State string `json:"state"`

	// Status is one of "pending", "running", "done", "failed", "skipped".
	Status string `json:"status"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`

	DurationSeconds float64 `json:"duration_seconds"`
	Duration        string  `json:"duration"`

	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// CheckDTO is the outcome of a single configured check.
type CheckDTO struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// Status is one of "pass", "fail", "error", "skipped".
	Status string `json:"status"`
	Pass   bool   `json:"pass"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`

	DurationSeconds float64 `json:"duration_seconds"`
	Duration        string  `json:"duration"`

	Attempts int            `json:"attempts"`
	Message  string         `json:"message,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// JSON writes run to w as the versioned Document described by this file's
// doc comment. It never marshals core types directly.
func JSON(w io.Writer, run *core.RecoveryRun) error {
	doc := toDocument(run)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func toDocument(run *core.RecoveryRun) Document {
	doc := Document{
		Schema: SchemaVersion,

		RunID:    run.ID,
		PlanName: run.PlanName,

		ProviderID:       run.ProviderID,
		BackupProviderID: run.BackupProviderID,

		SourceWorkloadID: run.SourceWorkloadID,
		SourceName:       run.SourceName,
		TempWorkloadID:   run.TempWorkloadID,
		TempName:         run.TempName,
		Node:             run.Node,

		Backup: toBackupDTO(run.Backup),

		State:  string(run.State),
		Result: string(run.Result),

		StartedAt:   run.StartedAt,
		CompletedAt: run.CompletedAt,

		Steps:  make([]StepDTO, 0, len(run.Steps)),
		Checks: make([]CheckDTO, 0, len(run.Checks)),

		RTOSeconds: run.RTO.Seconds(),
		RTO:        FormatDuration(run.RTO),

		RTOExceeded: run.RTOExceeded(),

		CleanupDone: run.CleanupDone,
		Error:       run.Err,
	}

	if run.RTOTarget > 0 {
		secs := run.RTOTarget.Seconds()
		human := FormatDuration(run.RTOTarget)
		doc.RTOTargetSeconds = &secs
		doc.RTOTarget = &human
	}

	for _, s := range run.Steps {
		doc.Steps = append(doc.Steps, toStepDTO(s))
	}
	for _, c := range run.Checks {
		doc.Checks = append(doc.Checks, toCheckDTO(c))
	}

	return doc
}

func toBackupDTO(b *core.Backup) *BackupDTO {
	if b == nil {
		return nil
	}
	return &BackupDTO{
		ID:         b.ID,
		WorkloadID: b.WorkloadID,
		ProviderID: b.ProviderID,
		Datastore:  b.Datastore,
		Node:       b.Node,

		CreatedAt:  b.CreatedAt,
		AgeSeconds: b.Age().Seconds(),
		Age:        FormatDuration(b.Age()),

		SizeBytes: b.SizeBytes,
		Size:      FormatBytes(b.SizeBytes),

		Protected: b.Protected,
		Encrypted: b.Encrypted,
		Verified:  string(b.Verified),
		Format:    b.Format,
		Notes:     b.Notes,
	}
}

func toStepDTO(s core.Step) StepDTO {
	return StepDTO{
		Name:  s.Name,
		State: string(s.State),

		Status: string(s.Status),

		StartedAt:   s.StartedAt,
		CompletedAt: s.CompletedAt,

		DurationSeconds: s.Duration.Seconds(),
		Duration:        FormatDuration(s.Duration),

		Message: s.Message,
		Error:   s.Err,
	}
}

func toCheckDTO(c core.CheckResult) CheckDTO {
	return CheckDTO{
		Name: c.Name,
		Type: c.Type,

		Status: string(c.Status),
		Pass:   c.OK(),

		StartedAt:   c.StartedAt,
		CompletedAt: c.CompletedAt,

		DurationSeconds: c.Duration.Seconds(),
		Duration:        FormatDuration(c.Duration),

		Attempts: c.Attempts,
		Message:  c.Message,
		Details:  c.Details,
	}
}
