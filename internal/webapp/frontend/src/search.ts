// Tiny cross-component signal: the sidebar's search controls ask whichever
// surface owns the ⌘K palette (Browser) to open it, optionally pre-filled —
// same in-repo-emitter spirit as nav.ts, no context plumbing through the shell.
type Listener = (query?: string) => void;
const listeners = new Set<Listener>();

export function onSearchRequest(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function requestSearch(query?: string): void {
  for (const l of listeners) l(query);
}
