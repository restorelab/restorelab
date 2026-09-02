import { useCallback, useEffect, useState } from "react"

type Theme = "light" | "dark"
const KEY = "restorelab.theme"

function preferred(): Theme {
  try {
    const stored = localStorage.getItem(KEY)
    if (stored === "light" || stored === "dark") return stored
  } catch {
    // A browser set to block site data throws on access. A dashboard is not
    // worth failing over a remembered preference.
  }
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light"
}

/** The viewer's theme: the system's choice unless they have overridden it. */
export function useTheme() {
  const [theme, setTheme] = useState<Theme>(preferred)

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
    try {
      localStorage.setItem(KEY, theme)
    } catch {
      // See above: the toggle still works for this page load.
    }
  }, [theme])

  const toggle = useCallback(() => {
    setTheme((t) => (t === "dark" ? "light" : "dark"))
  }, [])

  return { theme, toggle }
}
