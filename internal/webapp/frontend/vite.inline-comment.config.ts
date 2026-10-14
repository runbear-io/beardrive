import { defineConfig } from "vite";
import path from "node:path";

/* The commenting bootstrap, injected into the sandboxed iframe that renders a
   synced HTML file for comments (comments.go's commentSuffix). A classic IIFE
   script for exactly the reason vite.inline-edit.config.ts gives: the frame's
   origin is opaque, so a module load would need CORS.

   A pass of its own because Vite's lib mode builds one IIFE entry per run.
   Runs after the main build and must not empty the directory it filled. */
export default defineConfig({
  build: {
    outDir: "../static",
    emptyOutDir: false,
    lib: {
      entry: path.resolve(__dirname, "src/inline-comment.ts"),
      formats: ["iife"],
      name: "bdInlineComment",
      fileName: () => "inline-comment.js",
    },
  },
});
