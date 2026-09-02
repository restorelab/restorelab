import type { Provider, Workload } from "@/api/types"

/**
 * The provider a new plan should name.
 *
 * The one marked default, or the first configured. A plan must name its
 * provider - unlike an ad-hoc drill, where the server falls back to its own
 * default - so this has to resolve to something real rather than be left out.
 */
export function defaultProviderID(providers: Provider[]): string | undefined {
  return (providers.find((p) => p.default) ?? providers[0])?.id
}

/**
 * The document a new plan opens on.
 *
 * A blank textarea in front of a thirty-line nested format is hostile, so the
 * editor starts from a complete, commented plan for the machine that was
 * picked. Nothing here is guessed: the id and the name come from the workload
 * the dashboard already had, the provider from GET /providers, and everything
 * else is a constant of the format.
 *
 * It must pass POST /plans/validate. A skeleton that drifted from what the Go
 * side accepts would teach a format that does not exist - which has already
 * happened twice in this project, once with a CLI command the dashboard
 * printed and once with a diagnostic level no code emits. It happened a third
 * time while this file was being written: the first version left the provider
 * out, on the assumption that the server would resolve its default the way it
 * does for an ad-hoc drill. It does not. `workload.provider` is required, and
 * only running the real validator said so.
 */
export function planSkeleton(workload: Workload, providerID: string): string {
  return `# A recovery drill for ${workload.name} (${workload.id}).
#
# Restores its most recent backup onto an isolated bridge, boots it, proves it
# answers, then destroys the clone. Nothing about the production machine is
# touched.

name: ${workload.name}-drill
description: Recovery drill for ${workload.name}

workload:
  provider: ${providerID}
  id: "${workload.id}"

backup:
  # "latest" is the newest restore point. Add max_age to fail the drill when
  # the newest backup is older than you can accept - "your last backup is
  # three days old" is a finding, not a detail.
  strategy: latest

restore:
  network: isolated

checks:
  # ping, tcp, http, dns, or a command run inside the guest through the agent.
  - type: ping

cleanup:
  # The default anyway. Spelled out because destroying the clone is the
  # property that makes a drill safe to run against production backups.
  always: true

rto_target: 5m
`
}
