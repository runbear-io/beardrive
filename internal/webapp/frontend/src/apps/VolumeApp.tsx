import { useEffect, useMemo } from "react";
import type { ServerConfig } from "../api/types";
import { VaultHeader } from "../components/shell";
import { parseRoute } from "../router";
import { useLocationPath } from "../nav";
import Browser from "./Browser";

// Single-volume mode: one folder, no projects or orgs — but the full
// browsing surface (tree, listings, files, upload when enabled).
export default function VolumeApp({ config }: { config: ServerConfig }) {
  const pathname = useLocationPath();
  const name = config.volume || "BearDrive";
  useEffect(() => {
    document.title = config.brand || name;
  }, [config, name]);
  const route = useMemo(() => parseRoute(pathname, "volume"), [pathname]);

  return (
    <Browser
      config={config}
      apiBase="/api/"
      route={route}
      hub={false}
      sidebar={{ vault: <VaultHeader name={name} showSignout={config.auth.enabled} /> }}
    />
  );
}
