package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Attempts is the retry schedule: four tries over roughly ten minutes, as
// delays measured from the attempt before.
//
// The first entry is zero because the first attempt is immediate, which makes
// the slice indexable by "attempts already made" and leaves one place where
// the window is written down. Ten minutes is chosen against what the message
// is for: a drill that changed its verdict is worth waking somebody for now,
// and a channel that has been unreachable for ten minutes is broken in a way
// no further POST is going to fix.
var Attempts = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 8 * time.Minute}

// maxRetryAfter bounds how long a far end may park a delivery.
//
// A rate limited channel is asked to be believed, but only up to the window
// the schedule covers. A Retry-After of a day is a server telling us to hold a
// message until long after it stopped being news; honouring it would turn a
// visible failure into an invisible one, and the whole point of the failed
// state is that doctor can say the alerting path is broken.
const maxRetryAfter = 10 * time.Minute

// maxResponseBody bounds how much of an answer is read back off the wire.
//
// It is not an optimisation. A webhook URL is operator-supplied and can point
// at anything, including something that answers with a stream that never
// ends, and buffering that whole would take the dispatcher down rather than
// one delivery.
const maxResponseBody = 4 << 10

// errBodySnippet bounds how much of that answer is echoed into the error.
//
// It is a second, smaller bound for a different reason, on the model of
// proxmox.errBodyTruncateLen: the read limit protects the process, this one
// protects the delivery row and the log line an operator has to read. Four
// kilobytes of somebody's error page in a database column helps nobody.
const errBodySnippet = 512

// Result is what one POST established.
//
// Retry is derived from Err through core.IsRetryable rather than set beside
// it, so the two cannot disagree. Err is nil exactly when the message
// arrived.
//
// A caller has to tell one non-retryable error from another: Err wraps
// context.Canceled when the process itself is shutting down, and a delivery
// that ends that way must be left alone rather than recorded as a failure of
// the channel.
type Result struct {
	Status  int
	Err     error
	Retry   bool
	RetryAt time.Duration
}

// Sender posts rendered bodies to channel URLs.
//
// It holds one *http.Client, built once, for the reason proxmox.New builds
// one: a client per request leaks a connection pool per request, and this one
// runs on a ticker for the life of the process.
type Sender struct {
	hc *http.Client
}

// NewSender returns a Sender with its own client and transport. A timeout of
// zero or less gets the default.
func NewSender(timeout time.Duration) *Sender {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Sender{hc: &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{},
	}}
}

// Post delivers one body to one URL.
//
// target is a bearer credential and never appears in a returned error, which
// is what withoutURL is for. The caller logs the channel id instead, the way
// proxmox.request keeps its Authorization header out of every message it
// builds.
func (s *Sender) Post(ctx context.Context, target string, body []byte) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		// A URL that will not parse is a configuration error, not a transient
		// one. Asking again produces the same refusal.
		return Result{Err: fmt.Errorf("notify: build request: %w", withoutURL(err))}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.hc.Do(req)
	if err != nil {
		// Before anything else. A process shutting down cancels every context
		// it owns, and without this the whole pending queue would be recorded
		// as having been refused by channels that were working.
		if ctx.Err() != nil {
			return Result{Err: fmt.Errorf("notify: delivery abandoned: %w", ctx.Err())}
		}
		wrapped := core.Retryable(fmt.Errorf("notify: post: %w", withoutURL(err)))
		return Result{Err: wrapped, Retry: true}
	}
	defer func() { _ = resp.Body.Close() }()

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	r := Result{Status: resp.StatusCode, Err: classify(resp.StatusCode, snippet)}
	r.Retry = core.IsRetryable(r.Err)
	if r.Retry && resp.StatusCode == http.StatusTooManyRequests {
		r.RetryAt = retryAfter(resp.Header.Get("Retry-After"))
	}
	return r
}

// withoutURL strips the request URL out of a net/url failure.
//
// Everything in net/http and net/url reports a failure as a *url.Error, whose
// message repeats the whole URL that was being fetched: "Post
// \"https://discord.com/api/webhooks/1/<token>\": dial tcp: connection
// refused". For a Discord or Slack webhook the path is the credential, so
// that message is the secret, and the caller writes it into a delivery row
// and into a log line - two places that outlive the incident and get pasted
// into support threads. A database is disk in the sense invariant 8 means it.
//
// Only the cause is kept. What was being posted to is something the caller
// knows and states as a channel id; what went wrong is the only part this
// layer can contribute.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// classify decides whether asking again could produce a different answer.
//
// Invariant 7 forbids retrying Restore and Delete, and that reasoning does not
// transfer here: those are destructive, non-idempotent operations against a
// production cluster, and this is a POST to a chat webhook.
//
// The accepted risk is stated rather than hidden. A timeout on a request that
// actually succeeded produces a duplicate message, because from here a slow
// success and a lost request are indistinguishable. A duplicate is the better
// failure: it is noticed and ignored, while a silence is neither.
//
// Every 4xx except 429 is permanent. A revoked webhook, a deleted channel or a
// body the far end will not parse are all facts that a fifth POST will not
// change, and retrying them only delays the moment doctor can say the path is
// broken.
func classify(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	snippet := bytes.TrimSpace(body)
	if len(snippet) > errBodySnippet {
		snippet = snippet[:errBodySnippet]
	}
	base := fmt.Errorf("notify: refused with status %d: %s", status, snippet)
	if status >= 500 || status == http.StatusTooManyRequests {
		return core.Retryable(base)
	}
	return base
}

// retryAfter reads the header, in seconds, bounded by maxRetryAfter.
//
// Only the integer form is read. The HTTP-date form is legal and no channel
// this product speaks to uses it, and answering it would mean trusting a
// remote clock to schedule local work; a zero here simply hands the decision
// back to the schedule, which is the right answer for a header we did not
// understand.
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(header)
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// NextAttempt reports when to try again, given how many attempts have already
// been made.
//
// It returns false when the answer was permanent or the window is spent, and
// that false is what turns a delivery into a recorded failure. A queue that
// retries forever is a queue where nothing is ever known to be broken.
//
// A Retry-After from the far end wins over the schedule: it is the only party
// that knows when it will accept the message.
func NextAttempt(attempts int, r Result) (time.Duration, bool) {
	if !r.Retry || attempts < 0 || attempts >= len(Attempts) {
		return 0, false
	}
	if r.RetryAt > 0 {
		return r.RetryAt, true
	}
	return Attempts[attempts], true
}
