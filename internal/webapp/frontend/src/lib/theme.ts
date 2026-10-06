import { useSyncExternalStore } from "react";

// Light/dark theming for the hub SPA. The preference is per browser
// (localStorage), the resolved theme lives on <html data-theme> so CSS —
// style.css vars, tw.css --color-* overrides, Tailwind's `dark:` variant —
// keys off one attribute. index.html runs a tiny copy of resolve+apply
// before first paint so a light-OS user never sees a dark flash; this module
// takes over once the bundle loads and keeps it live (OS changes, other tabs).

export type ThemePref = "system" | "light" | "dark";
export type Theme = "light" | "dark";

const KEY = "bdrive-theme";
const EVENT = "bdrive-theme";
const DARK_QUERY = "(prefers-color-scheme: dark)";

// The choice when localStorage is unusable (private mode, blocked storage,
// sandboxed frames throw on access): honoured for this page's lifetime
// rather than silently ignored.
let memoryPref: ThemePref = "system";

export function getThemePref(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" || v === "system" ? v : "system";
  } catch {
    return memoryPref;
  }
}

export function resolveTheme(p: ThemePref): Theme {
  if (p !== "system") return p;
  try {
    return window.matchMedia(DARK_QUERY).matches ? "dark" : "light";
  } catch {
    return "dark"; // no matchMedia: the historical look
  }
}

export function currentTheme(): Theme {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

// Re-resolve, write <html data-theme>/color-scheme, and tell useTheme.
function applyAndNotify(): void {
  const t = resolveTheme(getThemePref());
  const root = document.documentElement;
  root.dataset.theme = t;
  root.style.colorScheme = t;
  window.dispatchEvent(new Event(EVENT));
}

export function setThemePref(p: ThemePref): void {
  memoryPref = p;
  try {
    // "system" is the default, so it is stored as the absence of a choice.
    if (p === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, p);
  } catch {
    // Unpersistable: memoryPref carries it for this page.
  }
  applyAndNotify();
}

let started = false;

export function initTheme(): void {
  if (started) return;
  started = true;
  applyAndNotify();
  // Subscribed once regardless of the preference; only "system" reacts, so
  // switching preferences needs no listener bookkeeping.
  try {
    window.matchMedia(DARK_QUERY).addEventListener("change", () => {
      if (getThemePref() === "system") applyAndNotify();
    });
  } catch {
    // No matchMedia: "system" resolves to dark and never changes.
  }
  // Another tab changed the preference. storage events fire only in the
  // OTHER tabs; a null key means localStorage.clear().
  window.addEventListener("storage", (e) => {
    if (e.key === KEY || e.key === null) applyAndNotify();
  });
}

function subscribe(cb: () => void): () => void {
  window.addEventListener(EVENT, cb);
  return () => window.removeEventListener(EVENT, cb);
}

// A string, so useSyncExternalStore's Object.is check sees a stable value
// between changes (a fresh object per call would re-render forever).
function snapshot(): string {
  return `${getThemePref()}:${currentTheme()}`;
}

export function useTheme(): { pref: ThemePref; theme: Theme; setPref(p: ThemePref): void } {
  const [pref, theme] = useSyncExternalStore(subscribe, snapshot, snapshot).split(":") as [ThemePref, Theme];
  return { pref, theme, setPref: setThemePref };
}
