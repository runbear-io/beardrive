// Tiny cross-component signal: the sidebar's search button asks whichever
// surface owns the ⌘K palette (Browser) to open it — same in-repo-emitter
// spirit as nav.ts, no context plumbing through the shell.
type Listener = () => void;
const listeners = new Set<Listener>();

export function onSearchRequest(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function requestSearch(): void {
  for (const l of listeners) l();
}
