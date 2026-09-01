package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// --- test doubles and helpers -----------------------------------------------

// flippingHistory lets a test change a run's state from inside a GetRun call.
//
// That is how a stream reaches a terminal run without waiting for anything:
// the state changes exactly when the loop looks at it, in the loop's own
// goroutine, so the test never sleeps and never races the handler.
type flippingHistory struct {
	*fakeHistory
	calls int
	flip  func(h *fakeHistory, call int)
}

func (f *flippingHistory) GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error) {
	f.calls++
	if f.flip != nil {
		f.flip(f.fakeHistory, f.calls)
	}
	return f.fakeHistory.GetRun(ctx, idOrPrefix)
}

// newStreamServer wires a server whose stream ticks as fast as the machine
// allows. The rhythm is a field precisely so that a test drives the loop
// instead of waiting a real second for every pass.
func newStreamServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, _ := newTestServer(t, opts)
	s.ssePoll = time.Millisecond
	return s
}

// streamRequest builds an authenticated request that asks for a stream.
func streamRequest(target string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+testSecret)
	r.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// serveStream serves r and returns once the handler has returned.
//
// The deadline is not a wait: it is only ever reached by a stream that failed
// to end, and it turns what would be a ten-minute hang into a named failure.
func serveStream(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(rec, r)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never ended")
	}
	return rec
}

// sseFrame is one event of a text/event-stream body. Comment lines - the
// heartbeat - carry no frame and are asserted on the raw body instead.
type sseFrame struct {
	id    string
	event string
	data  string
}

// parseSSE splits a stream body into its frames.
func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var frame sseFrame
		seen := false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"): // a comment: the heartbeat
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
				seen = true
			case strings.HasPrefix(line, "event: "):
				frame.event = strings.TrimPrefix(line, "event: ")
				seen = true
			case strings.HasPrefix(line, "data: "):
				frame.data = strings.TrimPrefix(line, "data: ")
				seen = true
			}
		}
		if seen {
			frames = append(frames, frame)
		}
	}
	return frames
}

// seedEvents gives a run n progress events, seq 1..n.
func seedEvents(h *fakeHistory, runID string, n int) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for seq := int64(1); seq <= int64(n); seq++ {
		h.events[runID] = append(h.events[runID], store.Event{
			Seq: seq, At: at.Add(time.Duration(seq) * time.Second),
			State: core.RunRestoring, Step: "restore", Status: core.StepRunning,
			Message: "restoring",
		})
	}
}

// --- tests ------------------------------------------------------------------

// Same endpoint, two representations: a dashboard follows live, a script
// loops over JSON. Accept decides.
func TestEventsServeJSONByDefaultAndSSEOnDemand(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID // seedRuns leaves the run SUCCESS: the stream ends by itself
	seedEvents(h, id, 3)
	s := newStreamServer(t, Options{History: h})

	// No Accept: the B1 JSON, byte for byte what its clients already parse.
	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/"+id+"/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var jsonPage page[eventDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &jsonPage); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(jsonPage.Items) != 3 || jsonPage.Items[0].Seq != 1 {
		t.Fatalf("JSON gave %+v, want three events starting at seq 1", jsonPage.Items)
	}

	// An explicit Accept: application/json must not change that answer.
	explicit := serveStream(t, s, streamRequest("/api/v1/recovery-runs/"+id+"/events",
		map[string]string{"Accept": "application/json"}))
	if explicit.Body.String() != rec.Body.String() {
		t.Errorf("Accept: application/json changed the body:\n got %s\nwant %s", explicit.Body, rec.Body)
	}

	// Accept: text/event-stream: the same events, as a stream.
	sse := serveStream(t, s, streamRequest("/api/v1/recovery-runs/"+id+"/events", nil))
	if sse.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", sse.Code, sse.Body)
	}
	if got := sse.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := sse.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache: a proxy must not buffer a stream", got)
	}

	frames := parseSSE(t, sse.Body.String())
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 3 progress and 1 done: %q", len(frames), sse.Body)
	}
	for i, want := range []string{"1", "2", "3"} {
		if frames[i].event != "progress" {
			t.Errorf("frame %d event = %q, want progress", i, frames[i].event)
		}
		// The id is the seq already stored in run_events: no translation,
		// which is what makes Last-Event-ID free.
		if frames[i].id != want {
			t.Errorf("frame %d id = %q, want %q", i, frames[i].id, want)
		}
	}
	var first eventDTO
	if err := json.Unmarshal([]byte(frames[0].data), &first); err != nil {
		t.Fatalf("frame data is not an event: %v", err)
	}
	if first.Seq != 1 || first.Step != "restore" {
		t.Errorf("first frame = %+v, want the seq 1 restore event", first)
	}
	if frames[3].event != "done" {
		t.Errorf("last frame event = %q, want done", frames[3].event)
	}
}

// The replay that makes a reconnect harmless: Last-Event-ID carries the seq
// already stored, and the stream resumes after it without repeating.
func TestSSEResumesFromLastEventID(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID
	seedEvents(h, id, 3)
	s := newStreamServer(t, Options{History: h})

	rec := serveStream(t, s, streamRequest("/api/v1/recovery-runs/"+id+"/events",
		map[string]string{"Last-Event-ID": "2"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	frames := parseSSE(t, rec.Body.String())
	var progress []sseFrame
	for _, f := range frames {
		if f.event == "progress" {
			progress = append(progress, f)
		}
	}
	if len(progress) != 1 {
		t.Fatalf("got %d progress frames, want only the one missed: %q", len(progress), rec.Body)
	}
	if progress[0].id != "3" {
		t.Errorf("resumed at id %q, want 3: nothing already delivered may repeat", progress[0].id)
	}
}

// A finished run closes the stream instead of holding the connection open
// forever.
func TestSSEClosesWhenTheRunIsOver(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID
	h.setState(id, core.RunRestoring, time.Time{}) // still going when the stream opens
	seedEvents(h, id, 1)

	// The worker writes the last transition without ever writing an event
	// for it: the stream must re-read the run to notice. Call 1 resolves the
	// run for the handler, call 2 is the loop's first look, and the run is
	// over by the second.
	flipping := &flippingHistory{fakeHistory: h, flip: func(f *fakeHistory, call int) {
		if call >= 3 {
			f.setState(id, core.RunSuccess, time.Date(2026, 9, 1, 10, 1, 0, 0, time.UTC))
		}
	}}
	s := newStreamServer(t, Options{History: flipping})

	// serveStream returning at all is the assertion: a stream waiting for a
	// terminal *event* would still be open.
	rec := serveStream(t, s, streamRequest("/api/v1/recovery-runs/"+id+"/events", nil))

	frames := parseSSE(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatalf("the stream said nothing: %q", rec.Body)
	}
	last := frames[len(frames)-1]
	if last.event != "done" {
		t.Fatalf("last frame = %+v, want a done frame closing the stream", last)
	}
	var end struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(last.data), &end); err != nil {
		t.Fatalf("done frame is not JSON: %v", err)
	}
	if end.State != string(core.RunSuccess) {
		t.Errorf("done state = %q, want %q", end.State, core.RunSuccess)
	}
}

// A client that goes away ends the stream, whatever the run is doing.
func TestSSEStopsWhenTheClientGoesAway(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID
	h.setState(id, core.RunRestoring, time.Time{}) // never terminal: only the client can end this
	s := newStreamServer(t, Options{History: h})

	ctx, cancel := context.WithCancel(context.Background())
	r := streamRequest("/api/v1/recovery-runs/"+id+"/events", nil).WithContext(ctx)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(rec, r)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream ignored its client going away and would hold the connection forever")
	}
}

// A quiet stream still says something: the heartbeat is what survives a
// reverse proxy's idle timeout.
func TestSSEHeartbeatsThroughAQuietStream(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID
	h.setState(id, core.RunRestoring, time.Time{})
	// No events at all: a heartbeat is the only thing this stream can send.

	flipping := &flippingHistory{fakeHistory: h, flip: func(f *fakeHistory, call int) {
		if call >= 3 {
			f.setState(id, core.RunSuccess, time.Date(2026, 9, 1, 10, 1, 0, 0, time.UTC))
		}
	}}

	// A clock that jumps a minute per reading, so the heartbeat is due on the
	// stream's first quiet pass without a real second going by. It is read
	// only from the goroutine serving the request.
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	reads := 0
	clock := func() time.Time {
		reads++
		return base.Add(time.Duration(reads) * time.Minute)
	}
	s := newStreamServer(t, Options{History: flipping, Now: clock})

	rec := serveStream(t, s, streamRequest("/api/v1/recovery-runs/"+id+"/events", nil))

	if !strings.Contains(rec.Body.String(), ": heartbeat\n\n") {
		t.Fatalf("a quiet stream sent no heartbeat: %q", rec.Body)
	}
}
