package checks

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// DNSCheck verifies a resolver answers a query, asking "does the restored
// resolver work?" by default (server defaults to the target's own IP).
//
// Params:
//   - name: name to query (required)
//   - server: DNS server to query (default: target.IP)
//   - port: DNS server port (default: 53)
//   - type: record type, one of A, AAAA, CNAME, MX, TXT (default: A)
//   - expect: list of acceptable answers; the check passes when the
//     returned answers include at least one of them (documented "any"
//     semantics, not "all")
type DNSCheck struct{}

func (DNSCheck) Type() string { return "dns" }

func (DNSCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	p := NewParams(cfg.Params, target)
	name := p.RequireString("name")
	server := p.String("server", target.IP)
	port := p.Int("port", 53)
	qtype := strings.ToUpper(p.String("type", "A"))
	expect := p.StringSlice("expect")
	if err := p.Err(); err != nil {
		return core.CheckResult{Status: core.CheckError, Message: err.Error()}
	}
	if strings.TrimSpace(server) == "" {
		return core.CheckResult{Status: core.CheckError, Message: "dns: no server (target has no IP and params.server is empty)"}
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(server, strconv.Itoa(port)))
		},
	}

	var answers []string
	var err error
	start := time.Now()
	switch qtype {
	case "A", "AAAA":
		var ips []net.IPAddr
		ips, err = resolver.LookupIPAddr(ctx, name)
		for _, ip := range ips {
			isV4 := ip.IP.To4() != nil
			if (qtype == "A") == isV4 {
				answers = append(answers, ip.IP.String())
			}
		}
	case "CNAME":
		var cname string
		cname, err = resolver.LookupCNAME(ctx, name)
		if err == nil {
			answers = append(answers, strings.TrimSuffix(cname, "."))
		}
	case "MX":
		var mxs []*net.MX
		mxs, err = resolver.LookupMX(ctx, name)
		for _, mx := range mxs {
			answers = append(answers, fmt.Sprintf("%s %d", strings.TrimSuffix(mx.Host, "."), mx.Pref))
		}
	case "TXT":
		var txts []string
		txts, err = resolver.LookupTXT(ctx, name)
		answers = append(answers, txts...)
	default:
		return core.CheckResult{Status: core.CheckError, Message: fmt.Sprintf("dns: unsupported record type %q (supported: A, AAAA, CNAME, MX, TXT)", qtype)}
	}
	latency := time.Since(start)

	if err != nil {
		details := map[string]any{"latency_ms": float64(latency) / float64(time.Millisecond)}
		// The resolver never answered, so nothing was learned about it: see
		// reachability.go. A resolver that answers with no records is a
		// different matter, handled below, and that one IS a failure.
		if cause, silent := dialFailure(err); silent {
			return core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("query %s %s via %s:%d: %s after %s - %s", qtype, name, server, port, cause, latency.Round(time.Millisecond), noRouteHint),
				Details: details,
			}
		}
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("query %s %s via %s:%d failed after %s: %v", qtype, name, server, port, latency.Round(time.Millisecond), err),
			Details: details,
		}
	}

	details := map[string]any{
		"answers":    answers,
		"latency_ms": float64(latency) / float64(time.Millisecond),
	}

	if len(answers) == 0 {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("query %s %s via %s:%d returned no answers", qtype, name, server, port),
			Details: details,
		}
	}

	if len(expect) == 0 {
		return core.CheckResult{
			Status:  core.CheckPass,
			Message: fmt.Sprintf("query %s %s via %s:%d -> %s", qtype, name, server, port, strings.Join(answers, ", ")),
			Details: details,
		}
	}

	var matched []string
	for _, want := range expect {
		if containsString(answers, want) {
			matched = append(matched, want)
		}
	}
	if len(matched) == 0 {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("query %s %s via %s:%d -> %s (expected one of %s)", qtype, name, server, port, strings.Join(answers, ", "), strings.Join(expect, ", ")),
			Details: details,
		}
	}
	return core.CheckResult{
		Status:  core.CheckPass,
		Message: fmt.Sprintf("query %s %s via %s:%d -> %s (matched %s)", qtype, name, server, port, strings.Join(answers, ", "), strings.Join(matched, ", ")),
		Details: details,
	}
}

var _ core.Check = DNSCheck{}
