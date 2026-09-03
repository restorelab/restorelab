package recovery

import (
	"fmt"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// maxTempNameLen mirrors the DNS-ish label limit Proxmox (and most
// hypervisors) enforce on VM/CT names.
const maxTempNameLen = 63

// tempWorkloadName builds the name of the temporary workload created for a
// run: "restorelab-<sourceID>-<YYYYMMDDHHMMSS>", sanitised to lowercase
// alphanumerics and dashes and truncated to fit maxTempNameLen. The
// timestamp comes last and is never truncated, since it is what keeps names
// unique across repeated runs against the same source workload.
func tempWorkloadName(sourceID string, at time.Time) string {
	slug := sanitizeSlug(sourceID)
	ts := at.UTC().Format("20060102150405")

	name := fmt.Sprintf("restorelab-%s-%s", slug, ts)
	if len(name) <= maxTempNameLen {
		return name
	}

	overflow := len(name) - maxTempNameLen
	if overflow >= len(slug) {
		slug = ""
	} else {
		slug = slug[:len(slug)-overflow]
	}
	slug = strings.Trim(slug, "-")

	if slug == "" {
		return fmt.Sprintf("restorelab-%s", ts)
	}
	return fmt.Sprintf("restorelab-%s-%s", slug, ts)
}

// sanitizeSlug lowercases s and keeps only [a-z0-9-], collapsing anything
// else (spaces, underscores, dots, ...) into a single dash.
func sanitizeSlug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// workloadMetadata is the ownership metadata stamped onto every temporary
// workload RestoreLab creates. Cleanup and any future "sweep orphans" job
// rely on these keys to prove a workload is theirs before touching it.
func workloadMetadata(runID, sourceID string, at time.Time) map[string]string {
	return map[string]string{
		core.MetadataManaged:       "true",
		core.MetadataRecoveryRunID: runID,
		core.MetadataSourceID:      sourceID,
		core.MetadataCreatedAt:     at.UTC().Format(time.RFC3339),
	}
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// criticalMap builds a check-name -> critical lookup from a plan, used to
// classify check failures when grading a run.
func criticalMap(p *plan.Plan) map[string]bool {
	m := make(map[string]bool, len(p.Checks))
	for _, c := range p.Checks {
		m[c.DisplayName()] = c.IsCritical()
	}
	return m
}

// humanBytes renders a byte count as a short MiB figure for error messages.
func humanBytes(b int64) string {
	return fmt.Sprintf("%d MiB", b/(1024*1024))
}

// stepEnd returns the completion time of the named step, or the zero time
// when the step never ran or never finished (still StepRunning, e.g. after a
// panic).
func stepEnd(run *core.RecoveryRun, name string) time.Time {
	for _, s := range run.Steps {
		if s.Name != name {
			continue
		}
		switch s.Status {
		case core.StepDone, core.StepFailed, core.StepSkipped:
			return s.CompletedAt
		}
	}
	return time.Time{}
}

// computeRTO implements the run's Recovery Time Objective measurement: the
// time from run start to the end of run_checks, or to the end of
// wait_for_guest when the plan has no checks. Cleanup and report generation
// happen after this point and are deliberately excluded: RTO measures how
// long the workload was actually down for, not how long RestoreLab spent
// tidying up its own scratch resources afterwards.
func computeRTO(run *core.RecoveryRun, startedAt time.Time) time.Duration {
	if end := stepEnd(run, StepRunChecks); !end.IsZero() {
		return end.Sub(startedAt)
	}
	if end := stepEnd(run, StepWaitForGuest); !end.IsZero() {
		return end.Sub(startedAt)
	}
	return 0
}

// checkMessage renders a short human line for a check-result event.
func checkMessage(r core.CheckResult) string {
	switch r.Status {
	case core.CheckPass:
		return fmt.Sprintf("check %q passed", r.Name)
	case core.CheckSkipped:
		return fmt.Sprintf("check %q skipped", r.Name)
	case core.CheckError:
		// Not "failed". A check that could not run says nothing about the
		// workload, and the timeline is where an operator forms their first
		// impression of what a drill found.
		if r.Message != "" {
			return fmt.Sprintf("check %q could not run: %s", r.Name, r.Message)
		}
		return fmt.Sprintf("check %q could not run", r.Name)
	default:
		if r.Message != "" {
			return fmt.Sprintf("check %q failed: %s", r.Name, r.Message)
		}
		return fmt.Sprintf("check %q failed", r.Name)
	}
}

// stepStatusForCheck maps a check outcome onto the StepStatus vocabulary
// Event uses, so the CLI can render both kinds of event the same way.
func stepStatusForCheck(s core.CheckStatus) core.StepStatus {
	switch s {
	case core.CheckPass:
		return core.StepDone
	case core.CheckSkipped:
		return core.StepSkipped
	default: // CheckFail, CheckError
		return core.StepFailed
	}
}
