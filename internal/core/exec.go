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
// guest through an out-of-band channel: the QEMU guest agent for Proxmox,
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
	// could not be run at all: no agent, no permission, guest not responding.
	// Providers return a wrapped ErrGuestAgentUnavailable in that last case so
	// callers can tell "your service is broken" apart from "I could not ask".
	ExecInGuest(ctx context.Context, workloadID string, req ExecRequest) (*ExecResult, error)
}

// GuestOS is what a guest agent reports about the operating system running
// inside the guest. Every field is best-effort: an agent that answers at all
// always fills Family, but the rest varies by agent version and by OS.
type GuestOS struct {
	// Family is the normalised OS family, lowercase: "windows" or "linux".
	// It is "" when the agent answered something this code does not
	// recognise, which callers must treat as "unknown", never as "linux".
	Family string
	// ID is the agent's own identifier ("debian", "mswindows", ...).
	ID string
	// Name is the human-readable OS name ("Debian GNU/Linux").
	Name string
	// PrettyName is the fullest human-readable form the agent has
	// ("Debian GNU/Linux 12 (bookworm)"), or "" when it reports none.
	PrettyName string
	// Version is the OS version as the agent words it ("12 (bookworm)").
	Version string
}

// Guest OS families reported in GuestOS.Family.
const (
	GuestOSWindows = "windows"
	GuestOSLinux   = "linux"
)

// GuestOSDetector is an optional companion to GuestExecutor, implemented by
// providers whose guest agent can also report what OS it is running on.
//
// It exists so that callers do not have to know our packaging conventions:
// a command check can pick /bin/sh or cmd on its own instead of making the
// operator remember that a Windows drill needs "shell: cmd" spelled out.
//
// Callers must type-assert for it and degrade gracefully when it is absent
// or fails: the guest is often still booting when the first check runs,
// and "I could not ask" is a normal, temporary answer, not a fatal one.
type GuestOSDetector interface {
	// GuestOS reports the OS running inside the guest. It returns a wrapped
	// ErrGuestAgentUnavailable when the agent could not be reached, the same
	// way ExecInGuest does.
	GuestOS(ctx context.Context, workloadID string) (GuestOS, error)
}

// String renders a guest OS for a human, in the fullest form the agent gave
// us. It is "unknown" when the agent reported nothing usable.
func (g GuestOS) String() string {
	switch {
	case g.PrettyName != "":
		return g.PrettyName
	case g.Name != "" && g.Version != "":
		return g.Name + " " + g.Version
	case g.Name != "":
		return g.Name
	case g.ID != "":
		return g.ID
	default:
		return "unknown"
	}
}
