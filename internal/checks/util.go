package checks

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// isPermissionError reports whether err looks like the OS denied a
// privileged operation (e.g. an unprivileged process opening a raw ICMP
// socket). It is deliberately liberal: the exact error shape differs across
// Linux, macOS and Windows.
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == syscall.EPERM || errno == syscall.EACCES) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "operation not permitted"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "access is denied"),
		strings.Contains(msg, "forbidden by its access permissions"):
		return true
	}
	return false
}

// truncate shortens s to at most max bytes, for compact log/report snippets.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
