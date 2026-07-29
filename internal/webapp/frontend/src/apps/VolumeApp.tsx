import { useEffect, useMemo } from "react";
import type { ServerConfig } from "../api/types";
import { VaultHeader } from "../components/shell";
import { parseRoute, urlForPath } from "../router";
import { Redirect, useLocationPath } from "../nav";
import Browser from "./Browser";

// Single-volume mode: one folder, no projects or orgs — but the full
// browsing surface (tree, listings, files, upload when enabled).
export default function VolumeApp({ config }: { config: ServerConfig }) {
  const loc = useLocationPath(); // pathname + search
  const name = config.volume || "BearDrive";
  useEffect(() => {
    document.title = config.brand || name;
  }, [config, name]);
  const route = useMemo(() => parseRoute(loc, "volume"), [loc]);

  // /notes/ is the same page as /notes — see the same guard in HubApp.
  if (route.trailingSlash && route.path) {
    return <Redirect to={urlForPath(route.path)} />;
  }

  return (
    <Browser
      config={config}
      apiBase="/api/"
      route={route}
      hub={false}
      sidebar={{ vault: <VaultHeader name={name} showSignout={config.auth.enabled} search /> }}
    />
  );
}
