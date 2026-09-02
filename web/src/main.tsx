import { UnauthorizedError } from "@/api/client"
import { QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { RouterProvider, createRouter } from "@tanstack/react-router"
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import "@/i18n"
import "./index.css"
import { routeTree } from "./routeTree.gen"

const queryClient = new QueryClient({
  queryCache: new QueryCache({
    // A token revoked mid-session, or the 12-hour absolute expiry falling,
    // surfaces as a 401 on whatever request happens next. One place notices,
    // and the cache is cleared rather than invalidated: data read under a dead
    // session must not stay on screen behind a login form.
    onError: (error) => {
      if (error instanceof UnauthorizedError) {
        queryClient.clear()
        void router.navigate({ to: "/login" })
      }
    },
  }),
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      retry: (count, error) => !(error instanceof UnauthorizedError) && count < 2,
    },
  },
})

const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
})

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById("root") as HTMLElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
