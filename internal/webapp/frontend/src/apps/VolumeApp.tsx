import { useEffect, useMemo } from "react";
import type { ServerConfig } from "../api/types";
import { VaultHeader } from "../components/shell";
import { parseRoute, titleForRoute, urlForPath } from "../router";
import { Redirect, useLocationPath } from "../nav";
import Browser from "./Browser";

// Single-volume mode: one folder, no projects or orgs — but the full
// browsing surface (tree, listings, files, upload when enabled).
export default function VolumeApp({ config }: { config: ServerConfig }) {
  const loc = useLocationPath(); // pathname + search
  const name = config.volume || "BearDrive";
  const route = useMemo(() => parseRoute(loc, "volume"), [loc]);
  const scope = config.brand || config.volume;
  useEffect(() => {
    document.title = scope ? titleForRoute(route, scope) : "BearDrive";
  }, [route, scope]);

  // /notes/ is the same page as /notes — see the same guard in HubApp.
  if (route.trailingSlash && route.path) {
    return <Redirect to={urlForPath(route.path, undefined, route.version, route.full, route.editing)} />;
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
