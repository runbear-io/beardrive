import { useSyncExternalStore } from "react";

// The sidebar's search field and the filtered tree are rendered by different
// owners (the project nav vs. the browser's tree slot), so the query and the
// highlighted hit live here, in the same in-repo-emitter spirit as search.ts.
// The tree registers how many rows it shows and how to open one; the field
// drives selection and activation from the keyboard.

interface State {
  query: string;
  sel: number;
}

let state: State = { query: "", sel: 0 };
let count = 0;
let activate: ((i: number) => void) | null = null;
const listeners = new Set<() => void>();

function set(next: State) {
  state = next;
  for (const l of listeners) l();
}

export function useSidebarSearch(): State {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => state,
  );
}

export function setFilterQuery(query: string) {
  if (query !== state.query) set({ query, sel: 0 });
}

export function clearFilter() {
  setFilterQuery("");
}

// `n` counts every selectable row, the "Search contents" footer included.
export function registerFilterResults(n: number, fn: (i: number) => void) {
  count = n;
  activate = fn;
  if (state.sel >= n) set({ ...state, sel: Math.max(0, n - 1) });
}

export function moveFilterSel(delta: number) {
  if (count === 0) return;
  set({ ...state, sel: (state.sel + delta + count) % count });
}

export function setFilterSel(i: number) {
  if (i !== state.sel) set({ ...state, sel: i });
}

export function activateFilterSel() {
  activate?.(state.sel);
}
