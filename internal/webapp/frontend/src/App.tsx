import { useEffect } from "react";
import { useConfig } from "./hooks/useConfig";

// Phase 0 shell: boots /api/config (redirecting to /auth/login when there
// is no session) and renders the static layout so the ported stylesheet can
// be verified. Routing and data views arrive in Phase 1.
export default function App() {
  const { data: config } = useConfig();

  useEffect(() => {
    if (config) document.title = config.brand || config.volume || "BearDrive";
  }, [config]);

  return (
    <>
      <div id="sb-backdrop" />
      <aside id="sidebar">
        <header id="vault">
          <span id="vault-badge" aria-hidden="true">
            🐻
          </span>
          <span id="vault-name">{config ? config.brand || config.volume || "BearDrive" : "…"}</span>
        </header>
        <nav id="tree" aria-label="Files" />
      </aside>
      <main id="main">
        <header id="topbar">
          <span id="crumb" />
          <span id="meta" />
        </header>
        <article id="content" className="markdown">
          <div className="empty">{config ? "Select a file to read it." : "Loading…"}</div>
        </article>
      </main>
    </>
  );
}
