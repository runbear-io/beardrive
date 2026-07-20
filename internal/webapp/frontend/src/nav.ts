import { useEffect, useSyncExternalStore, type MouseEvent } from "react";

// Minimal synchronous history router. React Router v7 wraps navigation in
// React.startTransition, which can leave the old view on screen for
// seconds after the URL changes (the transition gets starved by query
// updates) — the classic app's routing was synchronous, and parity needs
// it to stay that way. This is the whole router: pushState/replaceState +
// popstate, delivered through useSyncExternalStore.

export type NavType = "PUSH" | "REPLACE" | "POP";
let navType: NavType = "POP";
const listeners = new Set<() => void>();
function emit() {
  for (const l of listeners) l();
}
window.addEventListener("popstate", () => {
  navType = "POP";
  emit();
});

export function navigate(url: string, opts?: { replace?: boolean }) {
  // Skip a no-op push that would just stack a duplicate history entry
  // (e.g. when boot opens the file already in the URL).
  const cur = location.pathname + location.search;
  if (!opts?.replace && cur === url) return;
  history[opts?.replace ? "replaceState" : "pushState"](null, "", url);
  navType = opts?.replace ? "REPLACE" : "PUSH";
  emit();
}

export function useLocationPath(): string {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => {
        listeners.delete(l);
      };
    },
    () => location.pathname,
  );
}

// How the current location was reached — POP means back/forward, which is
// what scroll restoration keys off.
export function currentNavType(): NavType {
  return navType;
}

// The single place that decides whether an href is ours to route or the
// browser's to follow. Everything else just renders a link and lets the
// server say where it points — no component needs to know which kind of
// destination it got.
export function linkProps(href: string) {
  const internal = href.startsWith("/") && !href.startsWith("//");
  // An external destination leaves the app, so it opens in its own tab and
  // says so (callers render an ↗ off the same datum).
  if (!internal) return { href, target: "_blank", rel: "noopener noreferrer" };
  return {
    href,
    onClick: (e: MouseEvent) => {
      // Leave modified clicks (new tab/window, download) to the browser.
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
      e.preventDefault();
      navigate(href);
      // Navigating away closes the off-canvas sidebar. This lives here, not
      // in each link, because every in-app link needs it and the ones that
      // forget leave the drawer sitting on top of the page it just opened.
      document.body.classList.remove("sb-open");
    },
  };
}

// Render-time redirect (the declarative <Navigate replace> equivalent).
export function Redirect({ to }: { to: string }) {
  useEffect(() => {
    navigate(to, { replace: true });
  }, [to]);
  return null;
}
