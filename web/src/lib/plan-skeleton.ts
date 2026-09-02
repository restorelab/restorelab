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
 * It must pass POST /plans/validate, and the drill it describes must be one
 * that can actually succeed. Both were found the hard way:
 *
 *   - the first version left the provider out, on the assumption that the
 *     server resolves its default the way it does for an ad-hoc drill. It
 *     does not: `workload.provider` is required, and only running the real
 *     validator said so;
 *   - the second checked the clone with `ping`, which cannot reach a bridge
 *     that is isolated by design. The drill restored, booted and cleaned up
 *     perfectly and was graded FAILED on a check that could never pass.
 *
 * So the default check runs *inside* the guest through the QEMU agent, where
 * the isolation is not in the way. `hostname` is the one command that answers
 * on both Windows and Linux, which is why it is the one this project used to
 * validate its own Windows support.
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

startup:
  # The check below runs inside the guest, so the run waits for the agent to
  # answer rather than for an address on a network nothing can reach.
  wait_for_ip: false
  wait_for_agent: true

checks:
  # Proves the restored machine booted far enough to answer. Replace it with
  # what actually matters for this workload - "systemctl is-active postgresql",
  # a query against the database, a request to the application - because a
  # drill is only worth the check it ends on.
  - type: command
    name: guest responds
    run: hostname

cleanup:
  # The default anyway. Spelled out because destroying the clone is the
  # property that makes a drill safe to run against production backups.
  always: true

rto_target: 5m
`
}
