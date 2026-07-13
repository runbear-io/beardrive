import { useEffect } from "react";
import type { ServerConfig } from "../api/types";
import { AppShell, Topbar, VaultHeader } from "../components/shell";

// Single-volume mode: one folder, no projects or orgs. File browsing
// arrives in Phase 2.
export default function VolumeApp({ config }: { config: ServerConfig }) {
  const name = config.volume || "BearDrive";
  useEffect(() => {
    document.title = config.brand || name;
  }, [config, name]);

  return (
    <AppShell
      vault={<VaultHeader name={name} showSignout={config.auth.enabled} />}
      topbar={<Topbar />}
    >
      <div className="empty">Select a file to read it.</div>
    </AppShell>
  );
}
