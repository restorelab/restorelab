package core

import "errors"

// Sentinel errors shared across providers, engine and CLI. Providers wrap
// their transport errors with these so the engine can decide what is
// retryable without knowing anything about Proxmox.
var (
	// ErrNotFound is returned when a workload, node or backup does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNoBackup is returned when a workload has no restorable backup.
	ErrNoBackup = errors.New("no backup available")
	// ErrNotManaged is returned when a destructive operation targets a
	// resource RestoreLab did not create. This is a hard safety stop.
	ErrNotManaged = errors.New("resource is not managed by restorelab")
	// ErrNetworkNotIsolated is returned when the restore network cannot be
	// proven isolated from production.
	ErrNetworkNotIsolated = errors.New("restore network is not isolated")
	// ErrUnauthorized signals bad or insufficient credentials.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrInsufficientCapacity is returned when the target node cannot host the
	// restore.
	ErrInsufficientCapacity = errors.New("insufficient capacity")
	// ErrUnsupported is returned by providers for optional capabilities they
	// do not implement.
	ErrUnsupported = errors.New("unsupported operation")
	// ErrGuestAgentUnavailable is returned when a command could not be run
	// inside a guest: no agent installed, agent not started yet, or the API
	// refused the call. It is deliberately distinct from a command that ran
	// and failed - one means "your service is broken", the other means "I
	// could not ask".
	ErrGuestAgentUnavailable = errors.New("guest agent unavailable")
	// ErrTimeout is returned when an operation exceeds its deadline.
	ErrTimeout = errors.New("timeout")
	// ErrCancelled is returned when a run was cancelled by a user.
	ErrCancelled = errors.New("cancelled")
)

// RetryableError marks an error as safe to retry (transient API or network
// failure). Provider transports wrap 5xx/connection errors with it; restore
// corruption and integrity failures must never be wrapped.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err as retryable. It returns nil for a nil error.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &RetryableError{Err: err}
}

// IsRetryable reports whether err was explicitly marked retryable.
func IsRetryable(err error) bool {
	var re *RetryableError
	return errors.As(err, &re)
}
