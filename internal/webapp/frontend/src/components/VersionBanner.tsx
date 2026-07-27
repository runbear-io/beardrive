import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HistoryEntry } from "../api/types";
import { whoChanged } from "../util";
import { Icon } from "./shell";

/* Viewing a past version is an ordinary file page pinned to older bytes, so
   nothing about the page itself says the content is stale — this banner is
   what stops it misleading. It stays on screen the whole time ?v= is set.

   Provenance comes from the file's own history feed rather than a new API:
   the same query key HistoryView primes, so arriving from a history click
   costs no request. */
export function VersionBanner(props: {
  apiBase: string;
  path: string;
  version: string;
  onViewCurrent: () => void;
}) {
  const { apiBase, path, version } = props;
  const qs = "path=" + encodeURIComponent(path);
  const { data } = useQuery({
    queryKey: ["history", apiBase, qs, 200],
    queryFn: () => getJSON<{ entries: HistoryEntry[] }>(apiBase + "history?" + qs + "&n=200"),
    staleTime: 15_000,
  });
  // Newest match: the same content hash recurs if a file is reverted to
  // bytes it already had, and the latest of those is the one just clicked.
  const entry = data?.entries?.find((e) => e.blob === version);
  const who = entry ? whoChanged(entry) : "";
  const when = entry?.time ? new Date(entry.time).toLocaleString() : "";
  const dl =
    apiBase +
    "blob?sha=" + version +
    "&name=" + encodeURIComponent(path.split("/").pop() || path) +
    "&download=1";
  return (
    <div className="vbanner" role="status">
      <span className="vb-icon">
        <Icon name="clock" />
      </span>
      <div className="vb-text">
        <b>{[when && "Version from " + when, who && "by " + who].filter(Boolean).join(" ") || "Earlier version"}</b>
        <span>This is not the current file.</span>
      </div>
      <div className="vb-actions">
        <button className="ai-btn" onClick={props.onViewCurrent}>
          View current
        </button>
        <a className="ai-btn" download href={dl}>
          Download this version
        </a>
      </div>
    </div>
  );
}
