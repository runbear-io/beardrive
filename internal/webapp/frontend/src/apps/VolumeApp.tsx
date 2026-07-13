import { useEffect, useMemo } from "react";
import { useLocation } from "react-router-dom";
import type { ServerConfig } from "../api/types";
import { VaultHeader } from "../components/shell";
import { parseRoute } from "../router";
import Browser from "./Browser";

// Single-volume mode: one folder, no projects or orgs — but the full
// browsing surface (tree, listings, files, upload when enabled).
export default function VolumeApp({ config }: { config: ServerConfig }) {
  const location = useLocation();
  const name = config.volume || "BearDrive";
  useEffect(() => {
    document.title = config.brand || name;
  }, [config, name]);
  const route = useMemo(
    () => parseRoute(location.pathname, "volume"),
    [location.pathname],
  );

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
