//go:build unix

package checks

import "syscall"

const (
	connResetErrno   = syscall.ECONNRESET
	hostUnreachErrno = syscall.EHOSTUNREACH
	netUnreachErrno  = syscall.ENETUNREACH
)

// answeredErrnos: something at the far end replied, so the check reached a
// real conclusion even though it did not succeed.
var answeredErrnos = []syscall.Errno{
	syscall.ECONNREFUSED,
	connResetErrno,
}

// silentErrnos: nothing answered, so the check could not run.
var silentErrnos = []syscall.Errno{
	syscall.ETIMEDOUT,
	syscall.EHOSTDOWN,
	hostUnreachErrno,
	netUnreachErrno,
	syscall.ENETDOWN,
}
