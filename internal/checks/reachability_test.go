package checks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

// A refused connection is the one network failure that IS conclusive: a RST
// came back, so the host is there and nothing is listening on that port.
// Produced for real rather than synthesised, because the whole point of this
// classifier is that it survives contact with a real operating system.
func TestDialFailure_RefusedIsNotSilent(t *testing.T) {
	ln := listenTCP(t)
	host, port := hostPort(t, ln.Addr())
	ln.Close()

	var d net.Dialer
	_, err := d.DialContext(context.Background(), "tcp", fmt.Sprintf("%s:%d", host, port))
	if err == nil {
		t.Fatal("expected the dial to be refused")
	}

	cause, silent := dialFailure(err)
	if silent {
		t.Errorf("a refused connection was classified as silence: %q", cause)
	}
	if !strings.Contains(cause, "refused") {
		t.Errorf("cause = %q, want it to name the refusal", cause)
	}
}

// The errno tables have to survive being wrapped the way the net package
// actually wraps them - OpError around SyscallError around Errno - or they
// classify nothing at all in production while passing every direct test.
func TestDialFailure_UnwrapsThroughNetErrors(t *testing.T) {
	if len(silentErrnos) == 0 {
		t.Skip("no errno table on this platform")
	}
	wrapped := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: os.NewSyscallError("connectex", silentErrnos[0]),
	}

	cause, silent := dialFailure(wrapped)
	if !silent {
		t.Errorf("a wrapped unreachable errno was not recognised: %q", cause)
	}
	if strings.Contains(cause, "connectex") || strings.Contains(cause, "dial tcp") {
		t.Errorf("cause = %q, want the wrapping stripped", cause)
	}
}

// A deadline that expired says the same thing as the OS connect timeout:
// nothing answered in the time allowed.
func TestDialFailure_DeadlineIsSilent(t *testing.T) {
	if _, silent := dialFailure(fmt.Errorf("dial: %w", context.DeadlineExceeded)); !silent {
		t.Error("an expired deadline should count as nothing answering")
	}
}

// Cancellation is not silence about the target - it is a decision about the
// run - and it must not be dressed up as one.
func TestDialFailure_CancelIsNotSilent(t *testing.T) {
	if _, silent := dialFailure(fmt.Errorf("dial: %w", context.Canceled)); silent {
		t.Error("a cancelled run should not be reported as an unreachable target")
	}
}

// The conservative end. An error this build cannot classify is treated as a
// genuine failure: hiding real bad news is worse, for a tool that verifies
// backups, than reporting a failure that turns out to be a routing problem.
func TestDialFailure_UnknownIsNotSilent(t *testing.T) {
	cause, silent := dialFailure(errors.New("something nobody has seen before"))
	if silent {
		t.Error("an unclassifiable error must not be reported as inconclusive")
	}
	if cause != "something nobody has seen before" {
		t.Errorf("cause = %q, want the original error preserved", cause)
	}
}

func TestDialFailure_NilIsNothing(t *testing.T) {
	if cause, silent := dialFailure(nil); cause != "" || silent {
		t.Errorf("dialFailure(nil) = (%q, %v), want (\"\", false)", cause, silent)
	}
}

// Not every DNS failure is silence. A resolver that says "no such name" has
// answered the question that was asked, and that is a conclusion; one that
// never answers has not. Getting this backwards would let a check quietly
// stop reporting real misses.
func TestDialFailure_DNSNotFoundIsAnAnswer(t *testing.T) {
	err := &net.DNSError{Name: "nowhere.invalid", Err: "no such host", IsNotFound: true}
	cause, silent := dialFailure(err)
	if silent {
		t.Error("a resolver answering \"no such name\" answered; that is not silence")
	}
	if !strings.Contains(cause, "nowhere.invalid") {
		t.Errorf("cause = %q, want it to name the host", cause)
	}
}

func TestDialFailure_DNSTimeoutIsSilent(t *testing.T) {
	err := &net.DNSError{Name: "nowhere.invalid", Err: "i/o timeout", IsTimeout: true}
	if _, silent := dialFailure(err); !silent {
		t.Error("a resolver that never answered should count as nothing answering")
	}
}

// The two tables must stay disjoint. An errno in both would make the result
// depend on which loop ran first, which is exactly the kind of thing that
// looks fine until the day it decides a customer's report.
func TestErrnoTablesAreDisjoint(t *testing.T) {
	for _, a := range answeredErrnos {
		for _, s := range silentErrnos {
			if a == s {
				t.Errorf("errno %d is classified both ways", uintptr(a))
			}
		}
	}
}
