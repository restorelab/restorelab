import i18n from "i18next"
import { initReactI18next } from "react-i18next"
import common from "./locales/en/common.json"

/**
 * One locale, and every visible string behind t() from the first line.
 *
 * English matches the rest of the project - code, docs, CLI messages - and
 * matches what the API already emits: problem+json titles, phase names, check
 * names. A French interface would show English the moment a server error
 * surfaced.
 *
 * There is no language detector, because there is nothing to detect between.
 * Adding a locale later is a file and one line here; retrofitting t() across
 * fifty components is what this avoids.
 */
export const resources = {
  en: { common },
} as const

void i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  ns: ["common"],
  resources,
  interpolation: { escapeValue: false },
})

/**
 * Registers a screen's namespace.
 *
 * Each screen owns its own locale file so that two of them written in parallel
 * never touch the same JSON. The module that uses a namespace is the one that
 * registers it, at import time - without that call t() renders the raw key.
 */
export function addNamespace(ns: string, bundle: Record<string, unknown>): void {
  i18n.addResourceBundle("en", ns, bundle, true, true)
}

export default i18n
