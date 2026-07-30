// Product analytics, loaded only when the server asks for it.
//
// The hub sends `analytics` in /api/config exactly when a managed
// deployment configured one (webapp.AnalyticsConfig). A self-hosted hub sends
// nothing, so this module makes no third-party request and posthog-js never
// enters the bundle — that is why the library is fetched from PostHog's CDN
// at runtime instead of installed as a dependency: an OSS install must not
// carry a tracker it never runs.
//
// There is no call queue. The loader resolves in a few hundred milliseconds,
// long before anyone can click a thing worth recording, and the one event that
// races it — identify — is fired from onload.

import type { ServerConfig } from "./api/types";

type PostHog = {
  init(key: string, opts: Record<string, unknown>): void;
  identify(id: string, props?: Record<string, unknown>): void;
  capture(event: string, props?: Record<string, unknown>): void;
};

declare global {
  interface Window {
    posthog?: PostHog;
  }
}

let started = false;

export function initAnalytics(cfg: ServerConfig) {
  const a = cfg.analytics;
  if (!a?.key || started) return;
  started = true;

  const s = document.createElement("script");
  // PostHog serves the library from an assets subdomain beside the ingestion
  // host (us.i → us-assets.i). A self-hosted PostHog has no such split, and
  // the replace is a no-op there, which is the right answer for it too.
  s.src = a.host.replace(".i.posthog.com", "-assets.i.posthog.com") + "/static/array.js";
  s.async = true;
  s.onload = () => {
    const ph = window.posthog;
    if (!ph) return;
    ph.init(a.key, {
      api_host: a.host,
      // Pin the library's defaults: this loads whatever posthog-js is current
      // on the CDN, so an unpinned behavior change would arrive unannounced.
      defaults: "2026-05-30",
      // The app is a history-API SPA (nav.ts) — without this only the first
      // route of a session would count as a pageview.
      capture_pageview: "history_change",
      session_recording: {
        maskAllInputs: true,
        // ponytail: mask every text node, because in this product nearly all
        // of it is customer data — file names, folder names, document bodies,
        // project names. Replays show layout, clicks and navigation only. If
        // that proves too opaque to debug with, unmask specific chrome
        // (topbar, dialogs, empty states) rather than lowering this globally.
        maskTextSelector: "*",
      },
    });
    if (cfg.me) {
      ph.identify(cfg.me.email, {
        email: cfg.me.email,
        name: cfg.me.name,
        // Present only on a hub with billing; lets funnels split by plan
        // without a second source of truth.
        ...(cfg.billing ? { plan: cfg.billing.plan } : {}),
      });
    }
  };
  document.head.appendChild(s);
}

// A no-op until (and unless) analytics loaded. Every caller can fire blind.
export function track(event: string, props?: Record<string, unknown>) {
  window.posthog?.capture(event, props);
}
