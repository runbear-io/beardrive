/* The share page's only script. Injected by shares.go ONLY when the shared
   document contains a mermaid fence, so a diagram-free share page still
   downloads nothing.

   No framework owns this DOM, so the rendered string goes straight back into
   document.body. The page is served under `sandbox allow-scripts` with an
   opaque origin — a module script and its import() both need
   Access-Control-Allow-Origin on the static assets (server.go), which is why
   this is a module rather than one self-contained classic bundle. */
import { renderMermaid, DARK, LIGHT } from "./lib/mermaid";

// Picked once, at load: the shell's colours come from a prefers-color-scheme
// block, and mermaid bakes its palette in at render time.
const dark = matchMedia("(prefers-color-scheme: dark)").matches;

renderMermaid(document.body.innerHTML, dark ? DARK : LIGHT).then((html) => {
  document.body.innerHTML = html;
});
