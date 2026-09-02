import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import react from "@vitejs/plugin-react"
// vitest/config, not vite: the `test` key below does not typecheck against
// Vite's own defineConfig.
import { defineConfig } from "vitest/config"

export default defineConfig({
  plugins: [
    // Must precede react(): the router plugin does not work the other way round.
    //
    // A screen's test lives next to the screen, which puts it inside the routes
    // directory, where the generator expects every file to export a Route. It
    // warns once per test file otherwise - six lines of noise on every build
    // and in every CI log, saying nothing.
    tanstackRouter({
      target: "react",
      autoCodeSplitting: true,
      routeFileIgnorePattern: "\\.test\\.tsx?$",
    }),
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: { "@": path.resolve(__dirname, "./src") },
  },
  build: {
    // go:embed cannot reach above its own directory, and a package at the
    // module root would be publicly importable where everything else here is
    // internal. Vite refuses to empty an outDir outside its root without an
    // explicit emptyOutDir; `make ui` puts the .gitkeep back afterwards.
    outDir: "../internal/ui/dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
        configure: (proxy) => {
          // changeOrigin only fixes Host. The API's CSRF guard reads Origin,
          // and without this every write from the dev server is a 403 that
          // nothing explains.
          proxy.on("proxyReq", (proxyReq) => {
            proxyReq.setHeader("origin", "http://localhost:8080")
          })
        },
      },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
  },
})
