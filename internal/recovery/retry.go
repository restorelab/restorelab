package recovery

import (
	"context"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// retry calls fn up to attempts times. It only retries when the returned
// error is explicitly marked retryable via core.IsRetryable: a validation
// failure, a bad-credentials error or a task that genuinely failed must
// surface on the first try, never be masked behind a silent delay-and-repeat.
//
// It is a method (not a free function) so it sleeps through e.sleep, the
// same injectable clock the rest of the engine uses. That is what lets
// tests exercise retry/backoff paths without actually waiting.
func (e *Engine) retry(ctx context.Context, attempts int, backoff time.Duration, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !core.IsRetryable(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		if serr := e.sleep(ctx, backoff); serr != nil {
			// ctx was cancelled while backing off: report the original
			// provider error, it is more useful than "context cancelled".
			return err
		}
	}
	return err
}
