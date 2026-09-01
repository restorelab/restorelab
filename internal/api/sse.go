package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// sseHeartbeat keeps a quiet stream alive through a reverse proxy that would
// otherwise time it out.
const sseHeartbeat = 15 * time.Second

// ssePoll is how often the stream looks for new events. The engine writes
// them through the journal, into the same database this reads: polling is
// what lets the API and the worker be different processes.
const ssePoll = time.Second

// streamEvents serves a run's progress as text/event-stream.
//
// The id of each SSE event is the seq already stored in run_events, so
// Last-Event-ID needs no translation and a reconnect replays exactly what
// the client missed - the same Events(runID, afterSeq) query the JSON
// representation uses.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, run *core.RecoveryRun) {
	// Checked before anything is written: once the 200 is out, a problem
	// document can no longer be sent, and a stream nothing ever flushes
	// would look to the client like a connection that simply hangs.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, newProblem("streaming-unsupported", "Streaming is not available",
			http.StatusInternalServerError, "this server cannot flush a response"))
		return
	}

	after := int64(0)
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			after = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	lastBeat := s.now()

	for {
		events, err := s.history.Events(r.Context(), run.ID, after)
		if err != nil {
			return // the client sees the stream end; the run is unaffected
		}
		for _, e := range events {
			payload, err := json.Marshal(newEventDTO(e))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: progress\ndata: %s\n\n", e.Seq, payload)
			after = e.Seq
			lastBeat = s.now()
		}
		flusher.Flush()

		// Re-read the run rather than trusting the events: the last state
		// transition is written by the worker, and a stream that waited for
		// an event that will never come would hold the connection forever.
		current, err := s.history.GetRun(r.Context(), run.ID)
		if err == nil && current.State.Terminal() {
			fmt.Fprintf(w, "event: done\ndata: {\"state\":%q}\n\n", current.State)
			flusher.Flush()
			return
		}

		if s.now().Sub(lastBeat) >= s.sseHeartbeat {
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
			lastBeat = s.now()
		}

		// A fresh timer per pass rather than a ticker: since Go 1.23 an
		// unreferenced timer is collected without being stopped, so this
		// leaks nothing, and it spares this package the one deferred stop
		// that would show up in the grep guarding it against mutating
		// provider calls.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.ssePoll):
		}
	}
}
