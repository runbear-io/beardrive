import { defineConfig } from "vite";
import path from "node:path";

/* The click-to-edit bootstrap, built on its own because it has to be a CLASSIC
   script — the one bundle here that cannot be an ES module.

   It is injected into the sandboxed iframe that renders a synced HTML file
   (server.go's serveEditable). That iframe has no allow-same-origin, so its
   origin is opaque: a module script fetched from the hub would be a
   cross-origin module load, which needs CORS headers the hub has no business
   growing — and `Access-Control-Allow-Origin: null` is a footgun, not a fix.
   A classic script has no such rule and just loads.

   So: IIFE, one self-contained file, no code splitting, no imports at runtime.
   Everything ProseMirror needs is inlined.

   Runs as a SECOND pass after the main build, and must not empty the output
   directory the main build just filled.  */
export default defineConfig({
  build: {
    outDir: "../static",
    emptyOutDir: false,
    // Not minifying identifiers away entirely: this is the one bundle that
    // runs inside somebody else's document, so a stack trace naming it is
    // worth the bytes.
    lib: {
      entry: path.resolve(__dirname, "src/inline-edit.ts"),
      formats: ["iife"],
      name: "bdInlineEdit",
      fileName: () => "inline-edit.js",
    },
  },
});
