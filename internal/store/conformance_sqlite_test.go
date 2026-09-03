package store

import "testing"

// The conformance suite against the embedded engine. It needs nothing
// installed, so it runs on every `go test ./...` — which is what makes it a
// real guard against the two engines drifting apart, rather than one that
// only fires when someone remembers to set an environment variable.

func TestSQLiteRunConformance(t *testing.T) { RunConformance(t, newTestStore) }

func TestSQLiteStepsAndChecksConformance(t *testing.T) {
	StepsAndChecksConformance(t, newTestStore)
}

func TestSQLiteEventsConformance(t *testing.T) { EventsConformance(t, newTestStore) }

func TestSQLiteListConformance(t *testing.T) { ListConformance(t, newTestStore) }

func TestSQLiteTokensConformance(t *testing.T) { TokensConformance(t, newTestStore) }

func TestSQLiteSessionsConformance(t *testing.T) { SessionsConformance(t, newTestStore) }

func TestSQLitePlanConformance(t *testing.T) { PlanConformance(t, newTestStore) }

func TestSQLiteTempWorkloadConformance(t *testing.T) { TempWorkloadConformance(t, newTestStore) }

func TestSQLiteLastRunsConformance(t *testing.T) { LastRunsConformance(t, newTestStore) }

func TestSQLiteQueueWriteConformance(t *testing.T) { QueueWriteConformance(t, newTestStore) }

func TestSQLiteQueueClaimConformance(t *testing.T) { QueueClaimConformance(t, newTestStore) }

func TestQueueStatesMatchCoreTerminal(t *testing.T) { QueueStatesMatchCoreTerminal(t) }

func TestSQLiteScheduleConformance(t *testing.T) { ScheduleConformance(t, newTestStore) }
