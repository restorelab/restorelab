import "@testing-library/jest-dom/vitest"
// Without this, t() renders raw keys and every assertion on a visible label
// fails for a reason that has nothing to do with the component under test.
import "@/i18n"

// jsdom has no layout, so it throws on scrollTo. TanStack Router calls it on
// every navigation; without this stub the output is buried in stack traces
// that say nothing about the test.
window.scrollTo = () => {}
