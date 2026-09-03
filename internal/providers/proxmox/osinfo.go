package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/restorelab/restorelab/internal/core"
)

// Compile-time proof that Provider satisfies core.GuestOSDetector.
var _ core.GuestOSDetector = (*Provider)(nil)

// agentOSInfo is the response of /nodes/{node}/qemu/{vmid}/agent/get-osinfo.
// The guest agent fills what it knows and omits the rest, so every field is
// optional; only "id" is reliably present on both Linux and Windows agents.
type agentOSInfo struct {
	Result struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		PrettyName string `json:"pretty-name"`
		Version    string `json:"version"`
		VersionID  string `json:"version-id"`
		KernelName string `json:"kernel-name"`
	} `json:"result"`
}

// GuestOS asks the QEMU guest agent what operating system it is running on.
//
// The endpoint needs the same privilege as IP discovery
// (VM.GuestAgent.Audit on PVE 8+, VM.Monitor before), which RestoreLab
// already grants, so this costs no new permission.
func (p *Provider) GuestOS(ctx context.Context, workloadID string) (core.GuestOS, error) {
	node, kind, err := p.resolve(ctx, workloadID)
	if err != nil {
		return core.GuestOS{}, err
	}
	if kind != "qemu" {
		return core.GuestOS{}, fmt.Errorf("proxmox: workload %q is a %s, not a qemu VM: guest OS detection is QEMU-only: %w", workloadID, kind, core.ErrUnsupported)
	}

	raw, err := p.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/agent/get-osinfo", node, workloadID), nil)
	if err != nil {
		return core.GuestOS{}, mapAgentError(err)
	}

	var res agentOSInfo
	if err := json.Unmarshal(raw, &res); err != nil {
		return core.GuestOS{}, fmt.Errorf("proxmox: decode agent/get-osinfo response for %s: %w", workloadID, err)
	}

	r := res.Result
	return core.GuestOS{
		Family:     osFamily(r.ID, r.Name, r.KernelName),
		ID:         r.ID,
		Name:       r.Name,
		PrettyName: r.PrettyName,
		Version:    osVersion(r.Version, r.VersionID),
	}, nil
}

// osFamily normalises what the agent reports into "windows", "linux", or ""
// for anything else.
//
// The Windows agent answers id "mswindows" and name "Microsoft Windows"; a
// Linux agent answers the distribution's /etc/os-release ID ("debian",
// "ubuntu", "rhel", ...) and kernel-name "Linux". Rather than enumerate every
// distribution (a list that would be wrong the day a new one ships), this
// recognises Windows explicitly and treats a reported Linux kernel as Linux;
// anything else stays unknown, so callers fall back to their own default
// instead of guessing wrong.
func osFamily(id, name, kernelName string) string {
	switch {
	case strings.Contains(strings.ToLower(id), "windows"),
		strings.Contains(strings.ToLower(name), "windows"):
		return core.GuestOSWindows
	case strings.EqualFold(kernelName, "linux"):
		return core.GuestOSLinux
	default:
		// The agent answered, but with something that is neither Windows nor
		// a Linux kernel: a BSD, an illumos, a future agent. Unknown is the
		// honest answer; guessing "linux" here would send a /bin/sh at it.
		return ""
	}
}

// osVersion prefers the agent's worded version ("12 (bookworm)") and falls
// back to the bare id ("12") when it reports only that.
func osVersion(version, versionID string) string {
	if version != "" {
		return version
	}
	return versionID
}
