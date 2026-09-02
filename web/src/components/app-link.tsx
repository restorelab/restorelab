import { Link, useRouter } from "@tanstack/react-router"
import type { ReactNode } from "react"

/**
 * A link that also renders outside a router.
 *
 * TanStack's Link reads the router from context and cannot render without a
 * RouterProvider. Every screen here exports its markup as a plain component so
 * it can be tested without one - that is what makes those tests fast and
 * unconcerned with routing - so a bare Link would crash them.
 *
 * Rather than each screen inventing its own escape hatch, they all use this
 * one. Inside the application it is a real typed Link, with preloading and
 * client-side navigation; in a test with no router it degrades to the anchor
 * pointing at the same URL, which is what the tests assert on anyway.
 *
 * `to` is a closed union rather than TanStack's inferred route type: passing
 * that generic through a wrapper costs more than it buys for a handful of
 * routes, and a typo here still fails to compile.
 */
type Target =
  | {
      to: "/" | "/runs" | "/workloads" | "/doctor" | "/plans" | "/plans/new"
      params?: undefined
    }
  | { to: "/runs/$id" | "/workloads/$id"; params: { id: string } }
  | { to: "/plans/$ref"; params: { ref: string } }

export type AppLinkProps = Target & {
  className?: string
  children: ReactNode
}

/**
 * The address this link points at, with its parameter substituted in.
 *
 * The placeholder is found rather than named: routes here take one parameter
 * and it is not always called `$id` - the catalogue addresses a plan by
 * `$ref`. Naming it would mean editing this function for every route that
 * spells its parameter differently.
 */
export function hrefFor({ to, params }: Target): string {
  if (!params) return to
  const [value] = Object.values(params)
  return to.replace(/\$\w+/, encodeURIComponent(String(value)))
}

export function AppLink({ to, params, className, children }: AppLinkProps) {
  // useRouter's declared type never admits undefined, but with warn: false it
  // returns exactly that outside a provider. The truthiness check is the whole
  // reason this component exists; no cast is needed to write it.
  const router = useRouter({ warn: false })

  if (!router) {
    return (
      <a href={hrefFor({ to, params } as Target)} className={className}>
        {children}
      </a>
    )
  }

  return params ? (
    <Link to={to} params={params} className={className}>
      {children}
    </Link>
  ) : (
    <Link to={to} className={className}>
      {children}
    </Link>
  )
}
