package core

import "context"

// ExecRequest is a command to run inside a recovered guest.
type ExecRequest struct {
	// Argv is the command and its arguments, executed directly without a
	// shell. Callers that want shell semantics build the invocation
	// themselves ("/bin/sh", "-c", "...").
	Argv []string
	// Input is written to the command's standard input.
	Input string
	// MaxOutputBytes caps how much of each stream is kept. Zero means the
	// provider's default. A recovered guest can produce a lot of output and
	// none of it is worth exhausting memory over.
	MaxOutputBytes int
}

// ExecResult is what a guest command produced.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// Truncated reports that output was cut at MaxOutputBytes.
	Truncated bool
	// Signal is non-empty when the command was killed rather than exiting.
	Signal string
}

// GuestExecutor is implemented by providers that can run a command inside a
// guest through an out-of-band channel — the QEMU guest agent for Proxmox,
// VMware Tools elsewhere.
//
// This is what makes RestoreLab usable without a network path into the
// recovery network: validating that a service actually came back no longer
// requires routing to the isolated bridge, a DHCP server on it, or an agent
// deployed next to it. The command travels over the same API connection that
// drove the restore.
type GuestExecutor interface {
	// ExecInGuest runs a command inside the guest and waits for it to exit.
	//
	// A non-zero exit code is a successful call with a failing command, not an
	// error: it is returned in ExecResult.ExitCode. An error means the command
	// could not be run at all — no agent, no permission, guest not responding.
	// Providers return a wrapped ErrGuestAgentUnavailable in that last case so
	// callers can tell "your service is broken" apart from "I could not ask".
	ExecInGuest(ctx context.Context, workloadID string, req ExecRequest) (*ExecResult, error)
}
