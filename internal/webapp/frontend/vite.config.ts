import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// Build output feeds the Go binary: ../static is the go:embed target in
// server.go, and the compiled assets are committed so plain `go build`
// needs no Node. Run ./check-dist.sh to verify the committed output is
// fresh.
// Dev target: a locally running hub (`bdrive web` or the e2e harness).
const target = process.env.BDRIVE_DEV_PROXY || "http://localhost:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  build: {
    outDir: "../static",
    emptyOutDir: true,
    rollupOptions: {
      // Two entries: the SPA, and the one script the server-rendered /s/
      // share page loads when its document has a mermaid fence.
      input: {
        index: path.resolve(__dirname, "index.html"),
        "share-mermaid": path.resolve(__dirname, "src/share-mermaid.ts"),
      },
      output: {
        // Fixed name OUTSIDE assets/: sharedMarkdownShell is a const string
        // and can't know a content hash, and server.go marks assets/
        // immutable for a year — an unhashed file there would pin a stale
        // bundle in shared caches. At the static root it gets no-cache.
        // Mermaid's own chunks keep the hashed assets/ names.
        entryFileNames: (c) =>
          c.name === "share-mermaid" ? "[name].js" : "assets/[name]-[hash].js",
      },
    },
  },
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
