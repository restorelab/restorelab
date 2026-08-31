package checks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/restorelab/restorelab/internal/core"
)

// PingCheck verifies a host answers ICMP echo requests.
//
// Params:
//   - host: address to ping (default: target.IP)
//   - count: number of echo requests to send (default: 3)
//   - interval: delay between requests (default: 500ms)
//   - privileged: use a raw ICMP socket instead of unprivileged UDP ping
//     (default: false). Unprivileged ping needs no elevated permissions on
//     Linux but is not available on every OS.
type PingCheck struct{}

func (PingCheck) Type() string { return "ping" }

func (PingCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	p := NewParams(cfg.Params, target)
	host := p.String("host", target.IP)
	count := p.Int("count", 3)
	interval := p.Duration("interval", 500*time.Millisecond)
	privileged := p.Bool("privileged", false)
	if err := p.Err(); err != nil {
		return core.CheckResult{Status: core.CheckError, Message: err.Error()}
	}
	if strings.TrimSpace(host) == "" {
		return core.CheckResult{Status: core.CheckError, Message: "ping: no host (target has no IP and params.host is empty)"}
	}
	if count < 1 {
		count = 1
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	pinger, err := probing.NewPinger(host)
	if err != nil {
		return core.CheckResult{Status: core.CheckError, Message: fmt.Sprintf("ping %s: could not resolve host: %v", host, err)}
	}
	pinger.Count = count
	pinger.Interval = interval
	// Let the pinger finish on its own once it has had a fair chance to
	// collect replies, instead of relying solely on the outer (per-attempt)
	// context deadline to distinguish "no replies" from "still running".
	pinger.Timeout = interval*time.Duration(count) + 2*time.Second
	pinger.SetPrivileged(privileged)

	runErr := pinger.RunWithContext(ctx)
	stats := pinger.Statistics()
	details := map[string]any{
		"packets_sent": stats.PacketsSent,
		"packets_recv": stats.PacketsRecv,
		"avg_rtt_ms":   float64(stats.AvgRtt) / float64(time.Millisecond),
		"packet_loss":  stats.PacketLoss,
	}

	if runErr != nil {
		if isPermissionError(runErr) {
			return core.CheckResult{
				Status: core.CheckError,
				Message: fmt.Sprintf(
					"ping %s: %v (ICMP socket denied; set privileged: true and grant CAP_NET_RAW / run elevated, or leave unprivileged if your OS supports it)",
					host, runErr,
				),
				Details: details,
			}
		}
		if !errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, context.Canceled) {
			return core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("ping %s: %v", host, runErr),
				Details: details,
			}
		}
		// The outer context was cancelled/timed out before the pinger's own
		// timeout: fall through and judge on whatever replies arrived.
	}

	if stats.PacketsRecv == 0 {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("no replies from %s (%d/%d packets, 100%% loss)", host, stats.PacketsRecv, stats.PacketsSent),
			Details: details,
		}
	}

	return core.CheckResult{
		Status:  core.CheckPass,
		Message: fmt.Sprintf("%d/%d replies, avg %.2fms", stats.PacketsRecv, stats.PacketsSent, float64(stats.AvgRtt)/float64(time.Millisecond)),
		Details: details,
	}
}

var _ core.Check = PingCheck{}
