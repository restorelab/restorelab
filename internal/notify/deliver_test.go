package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postTo(t *testing.T, h http.HandlerFunc) Result {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewSender(2*time.Second).Post(context.Background(), srv.URL, []byte(`{"ok":true}`))
}

// TestPostClassifiesEveryAnswer. Each row is a decision about whether asking
// again could produce a different answer, and getting one wrong is expensive
// in both directions: a retried 404 delays the moment somebody learns the
// path is broken, and a permanent 503 loses a message to a server that was
// merely restarting.
func TestPostClassifiesEveryAnswer(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantRetry bool
		wantErr   bool
	}{
		// Discord answers 204 with no body, Slack answers 200 with "ok".
		// Both mean the message arrived.
		{name: "204 is sent", status: http.StatusNoContent},
		{name: "200 is sent", status: http.StatusOK, body: "ok"},

		{name: "500 is retryable", status: http.StatusInternalServerError, wantRetry: true, wantErr: true},
		{name: "502 is retryable", status: http.StatusBadGateway, wantRetry: true, wantErr: true},
		{name: "503 is retryable", status: http.StatusServiceUnavailable, wantRetry: true, wantErr: true},

		// A revoked webhook or a deleted channel will not start working
		// because we asked again. Retrying it only delays the moment somebody
		// is told the path is broken.
		{name: "404 is permanent", status: http.StatusNotFound, wantErr: true},
		{name: "403 is permanent", status: http.StatusForbidden, wantErr: true},
		{name: "400 is permanent", status: http.StatusBadRequest, body: "bad payload", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := postTo(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			if r.Status != tc.status {
				t.Errorf("Status = %d, want %d", r.Status, tc.status)
			}
			if (r.Err != nil) != tc.wantErr {
				t.Errorf("Err = %v, want an error: %v", r.Err, tc.wantErr)
			}
			if r.Retry != tc.wantRetry {
				t.Errorf("Retry = %v, want %v (err %v)", r.Retry, tc.wantRetry, r.Err)
			}
			if r.Err != nil && !strings.Contains(r.Err.Error(), "notify") {
				t.Errorf("error does not say which subsystem failed: %v", r.Err)
			}
		})
	}
}

func TestPostSendsTheBodyAsJSON(t *testing.T) {
	var gotBody, gotType, gotMethod string
	r := postTo(t, func(w http.ResponseWriter, req *http.Request) {
		gotMethod = req.Method
		gotType = req.Header.Get("Content-Type")
		buf := make([]byte, 64)
		n, _ := req.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusNoContent)
	})
	if r.Err != nil {
		t.Fatalf("Post: %v", r.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q; every one of the three channels rejects a body sent as anything else", gotType)
	}
	if gotBody != `{"ok":true}` {
		t.Errorf("body = %q, want the bytes it was handed verbatim", gotBody)
	}
}

// TestPostHonoursRetryAfterWithinReason. A rate limited channel tells us when
// to come back and we listen, up to a point: past maxRetryAfter the far end is
// asking us to hold a message longer than the message stays news, and the
// schedule takes over so the failure gets recorded instead.
func TestPostHonoursRetryAfterWithinReason(t *testing.T) {
	t.Run("a short Retry-After is honoured", func(t *testing.T) {
		r := postTo(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		if !r.Retry {
			t.Fatal("429 was not retryable")
		}
		if r.RetryAt != 3*time.Second {
			t.Errorf("RetryAt = %v, want 3s", r.RetryAt)
		}
	})

	t.Run("a day-long Retry-After is capped", func(t *testing.T) {
		r := postTo(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		if !r.Retry {
			t.Fatal("429 was not retryable")
		}
		if r.RetryAt > maxRetryAfter {
			t.Errorf("RetryAt = %v, which is longer than the %v this product is willing to hold a message",
				r.RetryAt, maxRetryAfter)
		}
	})

	t.Run("a 429 with no Retry-After falls back to the schedule", func(t *testing.T) {
		r := postTo(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
		if !r.Retry {
			t.Fatal("429 was not retryable")
		}
		if r.RetryAt != 0 {
			t.Errorf("RetryAt = %v, want zero so NextAttempt decides", r.RetryAt)
		}
	})
}

func TestPostTreatsATransportFailureAsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens on that port any more

	r := NewSender(2*time.Second).Post(context.Background(), url, []byte(`{}`))
	if r.Err == nil {
		t.Fatal("posting to a closed port succeeded")
	}
	if !r.Retry {
		t.Errorf("a refused connection is not retryable: %v", r.Err)
	}
	if r.Status != 0 {
		t.Errorf("Status = %d, want 0: nothing answered, so there is no status to report", r.Status)
	}
}

// TestPostDoesNotBlameTheChannelForOurOwnShutdown is the one that matters on
// the day a process is restarted. Without the ctx.Err() check before
// classification, every delivery in flight is recorded as a failure of a
// channel that was working perfectly, and doctor then tells an operator their
// alerting is broken because somebody ran systemctl restart.
func TestPostDoesNotBlameTheChannelForOurOwnShutdown(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() { close(blocked); srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	r := NewSender(10*time.Second).Post(ctx, srv.URL, []byte(`{}`))
	if r.Err == nil {
		t.Fatal("Post succeeded against a server that never answered")
	}
	if !errors.Is(r.Err, context.Canceled) {
		t.Errorf("Err = %v; a caller has to be able to tell its own shutdown from a broken channel", r.Err)
	}
	if r.Retry {
		t.Error("a cancelled context was marked retryable, so the loop would keep trying while shutting down")
	}
}

// TestPostBoundsTheBodyItReads. A webhook URL can be pointed at anything,
// including something that answers with a stream that does not end. The
// discriminating detail is that Post returns with the real status well before
// the client timeout: reading the whole body would instead hang until the
// deadline and report a transport error.
func TestPostBoundsTheBodyItReads(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(make([]byte, 64<<10))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	start := time.Now()
	r := NewSender(3*time.Second).Post(context.Background(), srv.URL, []byte(`{}`))
	elapsed := time.Since(start)

	if r.Status != http.StatusInternalServerError {
		t.Fatalf("Status = %d, want 500: the body was read past what was needed to classify it", r.Status)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Post took %v: it waited for a body that never ends", elapsed)
	}
	if len(r.Err.Error()) > 4<<10 {
		t.Errorf("the recorded error is %d bytes; a delivery row would carry the whole stream", len(r.Err.Error()))
	}
}

// TestNextAttemptWalksTheScheduleThenGivesUp. Giving up has to be a visible
// event, not a loop that quietly keeps trying: the row goes to failed with a
// reason, and that is what doctor reads.
func TestNextAttemptWalksTheScheduleThenGivesUp(t *testing.T) {
	retryable := Result{Status: 500, Err: errors.New("boom"), Retry: true}

	for made, want := range Attempts {
		got, ok := NextAttempt(made, retryable)
		if !ok {
			t.Fatalf("NextAttempt(%d) gave up while the schedule still had %v to offer", made, want)
		}
		if got != want {
			t.Errorf("NextAttempt(%d) = %v, want %v", made, got, want)
		}
	}

	if _, ok := NextAttempt(len(Attempts), retryable); ok {
		t.Errorf("NextAttempt(%d) offered a fifth attempt; the window is %d tries", len(Attempts), len(Attempts))
	}
}

func TestNextAttemptNeverRetriesAPermanentAnswer(t *testing.T) {
	permanent := Result{Status: 404, Err: errors.New("no such webhook")}
	if _, ok := NextAttempt(1, permanent); ok {
		t.Error("a 404 was rescheduled")
	}
}

func TestNextAttemptPrefersTheServersOwnAnswer(t *testing.T) {
	throttled := Result{Status: 429, Err: errors.New("slow down"), Retry: true, RetryAt: 45 * time.Second}
	got, ok := NextAttempt(1, throttled)
	if !ok {
		t.Fatal("a 429 was not rescheduled")
	}
	if got != 45*time.Second {
		t.Errorf("NextAttempt = %v, want the 45s the far end asked for rather than the schedule", got)
	}
}

// A transport failure must not carry the webhook URL out of this package.
//
// net/http reports one as a *url.Error whose message repeats the whole
// request URL, and for a Discord webhook the path is the credential. The
// dispatcher writes Result.Err into notification_deliveries.error, so a raw
// *url.Error would put a live webhook on disk, and doctor and
// GET /api/v1/doctor would then hand it back out.
func TestPostNeverPutsTheChannelURLInItsError(t *testing.T) {
	const token = "SUPERSECRETTOKEN"
	// Port 1 accepts nothing, so this fails in the transport and never
	// reaches a server that could answer.
	target := "https://127.0.0.1:1/api/webhooks/123/" + token

	r := NewSender(2*time.Second).Post(context.Background(), target, []byte(`{}`))
	if r.Err == nil {
		t.Fatal("posting to a closed port succeeded")
	}
	if !r.Retry {
		t.Errorf("a refused connection is not retryable: %v", r.Err)
	}
	if strings.Contains(r.Err.Error(), token) {
		t.Errorf("the webhook credential is in the error the dispatcher stores: %v", r.Err)
	}
	if strings.Contains(r.Err.Error(), target) {
		t.Errorf("the webhook url is in the error the dispatcher stores: %v", r.Err)
	}
}

// The same, for a URL that will not even build a request. url.Parse also
// reports through *url.Error, so this is the second door into the same leak.
func TestPostNeverPutsAnUnparseableURLInItsError(t *testing.T) {
	const token = "SUPERSECRETTOKEN"
	target := "https://example.com:notaport/hooks/" + token

	r := NewSender(2*time.Second).Post(context.Background(), target, []byte(`{}`))
	if r.Err == nil {
		t.Fatal("a url with an invalid port built a request")
	}
	if strings.Contains(r.Err.Error(), token) {
		t.Errorf("the webhook credential is in the error the dispatcher stores: %v", r.Err)
	}
}
