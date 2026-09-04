import { ThemeToggle } from "@/components/theme-toggle"
import { Link } from "@tanstack/react-router"
import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"

const NAV = [
  { to: "/", key: "app.nav.overview" },
  { to: "/runs", key: "app.nav.drills" },
  { to: "/workloads", key: "app.nav.workloads" },
  { to: "/plans", key: "app.nav.plans" },
  { to: "/doctor", key: "app.nav.doctor" },
  { to: "/settings", key: "app.nav.settings" },
] as const

export function AppShell({
  children,
  onSignOut,
}: {
  children: ReactNode
  onSignOut: () => void
}) {
  const { t } = useTranslation()
  return (
    <div className="min-h-dvh">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-4">
          <span className="font-semibold">{t("app.name")}</span>
          <nav className="flex items-center gap-4 text-sm">
            {NAV.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                activeOptions={{ exact: item.to === "/" }}
                className="text-muted-foreground transition-colors hover:text-foreground"
                activeProps={{ className: "text-foreground font-medium" }}
              >
                {t(item.key)}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-1">
            <ThemeToggle />
            <button
              type="button"
              onClick={onSignOut}
              className="px-2 text-muted-foreground text-sm hover:text-foreground"
            >
              {t("app.signOut")}
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">{children}</main>
    </div>
  )
}
