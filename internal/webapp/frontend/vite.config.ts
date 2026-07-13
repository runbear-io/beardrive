import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Build output feeds the Go binary: ../static is the go:embed target in
// server.go, and the compiled assets are committed so plain `go build`
// needs no Node. Run ./check-dist.sh to verify the committed output is
// fresh.
// Dev target: a locally running hub (`bdrive web` or the e2e harness).
const target = process.env.BDRIVE_DEV_PROXY || "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  build: { outDir: "../static", emptyOutDir: true },
  server: {
    proxy: {
      // Everything the Go server owns; the frontend itself only ever uses
      // root-absolute URLs, so prefix proxying is enough.
      "/api": target,
      "/auth": target,
      "^/s/": target,
    },
  },
});
