package checks

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// Why this file exists.
//
// RestoreLab restores a workload onto a deliberately isolated bridge - a
// network built so that nothing can reach the clone and the clone can reach
// nothing. A check that dials the guest from the machine running RestoreLab
// can therefore fail for two entirely different reasons:
//
//   - the service under test is down, or
//   - this machine has no route into the recovery network at all.
//
// Only the first is news about a backup. The second is news about the
// operator's topology, and reporting it as a failed recovery makes the report
// lie - which is the one thing a tool that exists to be believed cannot
// afford. core.CheckError already means "the check could not run"; these
// helpers are what let the network checks reach for it honestly.

// dialFailure classifies a failed network operation.
//
// cause is a short, admin-readable root cause, stripped of Go's verbose
// wrapping. silent reports whether nothing at the far end answered at all,
// which is what makes a check inconclusive rather than failed.
//
// An error we cannot classify is deliberately NOT silent: quietly swallowing
// real bad news is worse, for a tool that verifies backups, than crying wolf.
func dialFailure(err error) (cause string, silent bool) {
	if err == nil {
		return "", false
	}

	// A per-attempt deadline that expired means the same thing as the OS
	// connect timeout: nothing answered in the time allowed.
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out", true
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", false
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		for _, e := range answeredErrnos {
			if errno == e {
				return answeredCause(errno), false
			}
		}
		for _, e := range silentErrnos {
			if errno == e {
				return silentCause(errno), true
			}
		}
	}

	// net.Error.Timeout() is consulted after the errno tables, not before,
	// because it is not reliable everywhere: on Windows a dial to a
	// black-holed address returns WSAETIMEDOUT (10060) with Timeout() ==
	// false. Measured, not assumed. It still catches the platforms where it
	// does work, and costs nothing where it does not.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out", true
	}

	// A name that could not be resolved was never dialled - but only some DNS
	// failures are silence. A resolver that answers "no such name" has
	// answered, and that is a conclusion; one that never answers at all is
	// not. The DNS check leans on this distinction directly.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "no such host: " + dnsErr.Name, false
		}
		if dnsErr.IsTimeout {
			return "no answer resolving " + dnsErr.Name, true
		}
		return dnsErr.Err + ": " + dnsErr.Name, false
	}

	return err.Error(), false
}

func answeredCause(errno syscall.Errno) string {
	if errno == connResetErrno {
		return "connection reset"
	}
	return "connection refused"
}

func silentCause(errno syscall.Errno) string {
	switch errno {
	case hostUnreachErrno:
		return "no route to host"
	case netUnreachErrno:
		return "network unreachable"
	}
	return "no answer"
}

// noRouteHint is appended wherever a check reports that nothing answered.
//
// It names both explanations, in the order they are likely, because from here
// the two are genuinely indistinguishable: silence is silence. Saying so is
// more useful than picking one and being confidently wrong.
const noRouteHint = "nothing answered: either this machine has no route into the isolated recovery network, " +
	"or the guest never brought its own network up. A check of this kind can only work when RestoreLab runs " +
	"somewhere that can reach the recovery network; a cmd: check runs inside the guest and needs no route at all"
