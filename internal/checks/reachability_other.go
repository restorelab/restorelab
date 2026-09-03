//go:build !unix && !windows

package checks

import "syscall"

// Platforms with neither the POSIX nor the Winsock error set. Nothing is
// classified by number here, so dialFailure falls through to net.Error's own
// Timeout() and, failing that, treats the error as a genuine failure. That is
// the conservative end: a real failure is never hidden.
const (
	connResetErrno   = syscall.Errno(0)
	hostUnreachErrno = syscall.Errno(0)
	netUnreachErrno  = syscall.Errno(0)
)

var (
	answeredErrnos []syscall.Errno
	silentErrnos   []syscall.Errno
)
