import { Button } from "@/components/ui/button"
import { useTheme } from "@/hooks/useTheme"
import { Moon, Sun } from "lucide-react"
import { useTranslation } from "react-i18next"

export function ThemeToggle() {
  const { t } = useTranslation()
  const { theme, toggle } = useTheme()
  return (
    <Button variant="ghost" size="icon" onClick={toggle} aria-label={t("app.theme")}>
      {theme === "dark" ? <Sun className="size-4" /> : <Moon className="size-4" />}
    </Button>
  )
}
