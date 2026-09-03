//go:build windows

package checks

import "syscall"

// Winsock error numbers, by value.
//
// Windows does not map these onto the portable syscall constants: there,
// syscall.ECONNREFUSED is a Go-internal placeholder (536870934), not
// WSAECONNREFUSED (10061), so errors.Is against the portable names never
// matches anything a real dial returns. Nor is net.Error.Timeout() usable -
// measured on Windows 11 with Go 1.27, a 21-second dial to a black-holed
// address returns errno 10060 with Timeout() == false.
//
// Matching on the message text is not an option either: Windows localises it,
// so "connection refused" appears in the user's own language. The number is
// the only stable thing.
const (
	connResetErrno   = syscall.Errno(10054) // WSAECONNRESET
	hostUnreachErrno = syscall.Errno(10065) // WSAEHOSTUNREACH
	netUnreachErrno  = syscall.Errno(10051) // WSAENETUNREACH
)

// answeredErrnos: something at the far end replied, so the check reached a
// real conclusion even though it did not succeed.
var answeredErrnos = []syscall.Errno{
	syscall.Errno(10061), // WSAECONNREFUSED - a RST came back
	connResetErrno,
}

// silentErrnos: nothing answered, so the check could not run.
var silentErrnos = []syscall.Errno{
	syscall.Errno(10060), // WSAETIMEDOUT
	syscall.Errno(10064), // WSAEHOSTDOWN
	hostUnreachErrno,
	netUnreachErrno,
	syscall.Errno(10050), // WSAENETDOWN
}
