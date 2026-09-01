package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Compile-time proof that Provider satisfies core.GuestExecutor.
var _ core.GuestExecutor = (*Provider)(nil)

// defaultMaxOutputBytes caps each of stdout/stderr when the caller leaves
// ExecRequest.MaxOutputBytes at zero. A recovered guest can produce a lot of
// output (a runaway log dump, a verbose health check) and none of it is
// worth exhausting memory over.
const defaultMaxOutputBytes = 64 << 10 // 64 KiB

// agentUnavailablePhrases are substrings PVE's error bodies use (wording has
// varied a little across PVE versions) when a guest-exec call could not be
// serviced at all: no agent installed, agent not started, or the QMP
// channel to it isn't answering. Matched case-insensitively against the
// wrapped error text, which already carries a truncated copy of the
// response body (see errBodyTruncateLen in client.go).
var agentUnavailablePhrases = []string{
	"qemu guest agent is not running",
	"no qemu guest agent configured",
	"guest agent is not running",
	"qmp command 'guest-exec' failed",
	"qmp command 'guest-exec-status' failed",
}

// mapAgentError re-interprets an error returned by p.post/p.get for an
// agent/ endpoint into the vocabulary GuestExecutor callers expect.
//
// The heuristic, documented honestly: PVE reports "the guest agent could
// not service this call" (no agent installed, agent not started, QMP
// timeout) as a plain HTTP 500 with a free-text body — the exact status
// code it also uses for a genuine internal server error, with nothing else
// to tell the two apart. client.go's mapStatusError therefore wraps every
// 500 as core.Retryable, which is the right default everywhere else in
// this package but is actively wrong here: every call this file makes
// targets an agent/ endpoint, so any 500 it sees is overwhelmingly more
// likely to mean "there is no agent to ask" than "PVE had a transient
// fault", and retrying it just burns the caller's deadline waiting on a
// guest that will never answer. So: match known phrasing in the body when
// present, and unconditionally treat any HTTP 5xx from these endpoints as
// agent-unavailable even when the body doesn't match a known phrase.
// Connection failures and timeouts (no HTTP status at all — p.request
// never gets far enough to call mapStatusError) carry no "unexpected
// status" text and no agent phrase, so they fall through unchanged and
// stay core.Retryable, per contract.
func mapAgentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrUnauthorized) {
		// %v, not %w, on err: this function deliberately re-labels the API
		// error rather than extending its chain. See the agentDown branch
		// below for why that matters; keeping both branches consistent is
		// what stops someone "fixing" only the harmless one.
		//nolint:errorlint // err is flattened on purpose, see above
		return fmt.Errorf("%w: guest agent exec requires the VM.Monitor privilege on this VM, and some PVE versions restrict it to root@pam: %v", core.ErrUnauthorized, err)
	}

	msg := strings.ToLower(err.Error())
	agentDown := strings.Contains(msg, "unexpected status 5")
	if !agentDown {
		for _, phrase := range agentUnavailablePhrases {
			if strings.Contains(msg, phrase) {
				agentDown = true
				break
			}
		}
	}
	if agentDown {
		// The advice is a hypothesis, not a diagnosis: PVE answers a genuine
		// server fault and a missing agent with the same untyped 500. The
		// original API error is appended so the operator can see which it is.
		//
		// %v, not %w, and that is load-bearing. The 5xx we are re-labelling
		// arrived wrapped in core.RetryableError; wrapping it with %w would
		// put that marker back in the chain and core.IsRetryable would start
		// returning true, so the engine would spend the caller's whole
		// deadline re-asking a guest agent that is not there. The contract
		// above says agent-unavailable is terminal: the text is carried over,
		// the retryable marker is not.
		//nolint:errorlint // err is flattened on purpose, see above
		return fmt.Errorf("%w: the guest agent did not answer - install and start qemu-guest-agent inside the guest, and enable \"QEMU Guest Agent\" under the VM's Options in Proxmox; if it is already running, the API error is the real cause: %v", core.ErrGuestAgentUnavailable, err)
	}
	return err
}

// ExecInGuest runs req.Argv inside workloadID's guest through the QEMU
// guest agent and blocks until it exits. See core.GuestExecutor for the
// error-vs-nonzero-exit contract.
func (p *Provider) ExecInGuest(ctx context.Context, workloadID string, req core.ExecRequest) (*core.ExecResult, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("proxmox: ExecInGuest: req.Argv is empty")
	}

	node, kind, err := p.resolve(ctx, workloadID)
	if err != nil {
		return nil, err
	}
	if kind != "qemu" {
		return nil, fmt.Errorf("proxmox: workload %q is a %s, not a qemu VM: guest agent exec is QEMU-only: %w", workloadID, kind, core.ErrUnsupported)
	}

	pid, err := p.agentExecStart(ctx, node, workloadID, req)
	if err != nil {
		return nil, mapAgentError(err)
	}
	return p.agentExecWait(ctx, node, workloadID, pid, req.MaxOutputBytes)
}

// agentExecStart issues the guest-exec call and returns the guest agent's
// pid for it.
func (p *Provider) agentExecStart(ctx context.Context, node, id string, req core.ExecRequest) (int, error) {
	// PVE takes the command as a repeated form field ("command=a&command=b&
	// command=c"), one value per argv element, not as a single
	// space-joined or JSON-encoded string. url.Values naturally supports a
	// repeated key with ordered values, and Values.Encode preserves that
	// order for a given key (it only sorts across distinct keys) — assign
	// req.Argv directly rather than building it with repeated Add calls.
	form := url.Values{"command": req.Argv}
	if req.Input != "" {
		form.Set("input-data", req.Input)
	}

	raw, err := p.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/agent/exec", node, id), form)
	if err != nil {
		return 0, err
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("proxmox: decode agent/exec response for %s: %w", id, err)
	}
	// PVE is inconsistent about whether pid travels as a JSON number or a
	// numeric string; asInt tolerates both.
	return asInt(res["pid"]), nil
}

// agentExecWait polls agent/exec-status for pid every p.pollInterval,
// honouring ctx cancellation the same way WaitForTask does, until the
// guest agent reports the command exited.
func (p *Provider) agentExecWait(ctx context.Context, node, id string, pid int, maxOutputBytes int) (*core.ExecResult, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%s/agent/exec-status", node, id)
	params := url.Values{"pid": {strconv.Itoa(pid)}}

	for {
		raw, err := p.get(ctx, path, params)
		if err != nil {
			return nil, mapAgentError(err)
		}
		var s map[string]any
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("proxmox: decode agent/exec-status response for %s: %w", id, err)
		}

		// PVE reports "exited" as the number 0/1, not a JSON bool.
		if asBool(s["exited"]) {
			return buildExecResult(s, maxOutputBytes), nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
}

// buildExecResult maps a settled agent/exec-status response into
// core.ExecResult, applying the MaxOutputBytes cap on top of whatever
// truncation PVE itself already reports.
func buildExecResult(s map[string]any, maxOutputBytes int) *core.ExecResult {
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}

	res := &core.ExecResult{
		ExitCode:  asInt(s["exitcode"]),
		Stdout:    asString(s["out-data"]),
		Stderr:    asString(s["err-data"]),
		Truncated: asBool(s["out-truncated"]) || asBool(s["err-truncated"]),
		Signal:    asString(s["signal"]),
	}

	if truncateTo(&res.Stdout, maxOutputBytes) {
		res.Truncated = true
	}
	if truncateTo(&res.Stderr, maxOutputBytes) {
		res.Truncated = true
	}
	return res
}

// truncateTo cuts *s down to at most max bytes in place, reporting whether
// it had to.
func truncateTo(s *string, max int) bool {
	if len(*s) <= max {
		return false
	}
	*s = (*s)[:max]
	return true
}
