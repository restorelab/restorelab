package checks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// TCPCheck verifies that a TCP endpoint accepts connections, optionally
// requiring the server's opening banner to contain a substring.
//
// Params:
//   - host: address to connect to (default: target.IP)
//   - port: TCP port (required)
//   - expect_banner: substring the server must send within the check's
//     timeout (optional; when unset, a successful connect is enough)
type TCPCheck struct{}

func (TCPCheck) Type() string { return "tcp" }

func (TCPCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	p := NewParams(cfg.Params, target)
	host := p.String("host", target.IP)
	port := p.RequireInt("port")
	expectBanner := p.String("expect_banner", "")
	if err := p.Err(); err != nil {
		return core.CheckResult{Status: core.CheckError, Message: err.Error()}
	}
	if strings.TrimSpace(host) == "" {
		return core.CheckResult{Status: core.CheckError, Message: "tcp: no host (target has no IP and params.host is empty)"}
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	latency := time.Since(start)
	if err != nil {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("%s on %s after %s", describeDialErr(err), addr, formatLatency(latency)),
			Details: map[string]any{"latency_ms": float64(latency) / float64(time.Millisecond)},
		}
	}
	defer func() { _ = conn.Close() }()

	details := map[string]any{"latency_ms": float64(latency) / float64(time.Millisecond)}

	if expectBanner == "" {
		return core.CheckResult{
			Status:  core.CheckPass,
			Message: fmt.Sprintf("connected to %s in %s", addr, formatLatency(latency)),
			Details: details,
		}
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	buf := make([]byte, 4096)
	reader := bufio.NewReader(conn)
	n, rerr := reader.Read(buf)
	banner := string(buf[:n])
	details["banner"] = banner

	if n == 0 {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("connected to %s but no banner received: %v", addr, rerr),
			Details: details,
		}
	}
	if !strings.Contains(banner, expectBanner) {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("connected to %s but banner %q does not contain %q", addr, truncate(banner, 120), expectBanner),
			Details: details,
		}
	}
	return core.CheckResult{
		Status:  core.CheckPass,
		Message: fmt.Sprintf("connected to %s, banner matched %q", addr, expectBanner),
		Details: details,
	}
}

// describeDialErr turns a raw dial error into a short, admin-friendly root
// cause, stripping Go's verbose "dial tcp host:port: " wrapping.
func describeDialErr(err error) string {
	if err == nil {
		return ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "no route to host"):
		return "no route to host"
	case strings.Contains(msg, "network is unreachable"):
		return "network unreachable"
	default:
		return msg
	}
}

var _ core.Check = TCPCheck{}

// formatLatency keeps a sub-millisecond connect readable: rounding to the
// millisecond turns a genuinely fast local connection into "0s", which reads
// like a bug in the report rather than a good result.
func formatLatency(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}
